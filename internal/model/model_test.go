package model

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func svc(image string, replicas int) Service {
	return Service{Replicas: replicas, Config: Config{Image: image}}
}

func TestConfigHashIgnoresReplicas(t *testing.T) {
	if ConfigHash(svc("nginx:1", 1)) != ConfigHash(svc("nginx:1", 5)) {
		t.Fatal("replica count changed the hash")
	}
	if ConfigHash(svc("nginx:1", 1)) == ConfigHash(svc("nginx:2", 1)) {
		t.Fatal("image change did not change the hash")
	}
	a, b := svc("nginx:1", 1), svc("nginx:1", 1)
	b.Expose = &Expose{HostPort: 8000, ContainerPort: 80}
	if ConfigHash(a) == ConfigHash(b) {
		t.Fatal("expose change did not change the hash")
	}
}

func TestConfigHashIgnoresExposedHostPort(t *testing.T) {
	a, b, c := svc("nginx:1", 1), svc("nginx:1", 1), svc("nginx:1", 1)
	a.Expose = &Expose{HostPort: 8000, ContainerPort: 80}
	b.Expose = &Expose{HostPort: 9000, ContainerPort: 80}
	c.Expose = &Expose{HostPort: 8000, ContainerPort: 8080}
	if ConfigHash(a) != ConfigHash(b) {
		t.Fatal("host port change changed the hash: the load balancer's port is not in the containers")
	}
	if ConfigHash(a) == ConfigHash(c) {
		t.Fatal("container port change did not change the hash")
	}
	if a.Expose.HostPort != 8000 {
		t.Fatal("ConfigHash modified the service")
	}
}

func TestWithSalt(t *testing.T) {
	if WithSalt("abc", "") != "abc" {
		t.Fatal("empty salt changed the hash")
	}
	if WithSalt("abc", "x") == "abc" || WithSalt("abc", "x") == WithSalt("abc", "y") {
		t.Fatal("salt did not change the hash")
	}
}

func TestConfigJSON(t *testing.T) {
	// compose-go wrote durations as "5s" and memory as a decimal string.
	var c Config
	in := `{"healthcheck":{"interval":"5s","retries":3},"deploy":{"resources":{"limits":{"cpus":0.5,"memory":"268435456"}}}}`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.HealthCheck.Interval != Duration(5*time.Second) || c.Deploy.Resources.Limits.Memory != 256<<20 || c.Deploy.Resources.Limits.CPUs != 0.5 {
		t.Fatalf("decoded %+v %+v", c.HealthCheck, c.Deploy.Resources.Limits)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip: %s, %v", b, err)
	}
}

func TestCreatesSource(t *testing.T) {
	no := false
	for _, tt := range []struct {
		v    Volume
		want bool
	}{
		{Volume{Type: VolumeBind, Bind: &Bind{}}, true},                      // short syntax, Compose's default
		{Volume{Type: VolumeBind}, false},                                    // long syntax without a bind block
		{Volume{Type: VolumeBind, Bind: &Bind{CreateHostPath: &no}}, false},  // create_host_path: false
		{Volume{Type: VolumeVolume, Source: "p_data", Bind: &Bind{}}, false}, // not a bind
	} {
		if got := tt.v.CreatesSource(); got != tt.want {
			t.Errorf("%+v: CreatesSource = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestConfigNullEnvAndEmptyEntrypoint(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"environment":{"A":"1","UNSET":null},"entrypoint":[]}`), &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Environment["UNSET"]; ok || c.Environment["A"] != "1" {
		t.Fatalf("env = %v: compose-go stored unresolved keys as null; they must stay unset", c.Environment)
	}
	b, _ := json.Marshal(c)
	var back Config
	json.Unmarshal(b, &back)
	if back.Entrypoint == nil {
		t.Fatalf("entrypoint [] lost in a state round trip: %s", b)
	}
}
