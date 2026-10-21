package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is what a service's containers are created from. The JSON names
// match the Compose keys, which is also how state files written before
// anastasis had its own loader stored them, so those files still load.
type Config struct {
	Image       string            `json:"image,omitempty"`
	Command     []string          `json:"command"`    // nil: the image's CMD; empty: none
	Entrypoint  []string          `json:"entrypoint"` // nil: the image's ENTRYPOINT; empty: none
	Environment map[string]string `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	User        string            `json:"user,omitempty"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	StopSignal  string            `json:"stop_signal,omitempty"`
	CapAdd      []string          `json:"cap_add,omitempty"`
	CapDrop     []string          `json:"cap_drop,omitempty"`
	ReadOnly    bool              `json:"read_only,omitempty"`
	HealthCheck *HealthCheck      `json:"healthcheck,omitempty"`
	Deploy      *Deploy           `json:"deploy,omitempty"`
	Ports       []Port            `json:"ports,omitempty"`
	Volumes     []Volume          `json:"volumes,omitempty"`
}

// UnmarshalJSON drops null environment values: compose-go wrote an
// unresolved bare KEY as null, which meant "leave unset", not "set empty".
func (c *Config) UnmarshalJSON(b []byte) error {
	type plain Config
	aux := struct {
		*plain
		Environment map[string]*string `json:"environment"`
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.Environment = nil
	for k, v := range aux.Environment {
		if v == nil {
			continue
		}
		if c.Environment == nil {
			c.Environment = map[string]string{}
		}
		c.Environment[k] = *v
	}
	return nil
}

// HealthCheck is a service's healthcheck. Disable also turns off the
// image's own HEALTHCHECK.
type HealthCheck struct {
	Test        []string `json:"test,omitempty"`
	Interval    Duration `json:"interval,omitempty"`
	Timeout     Duration `json:"timeout,omitempty"`
	StartPeriod Duration `json:"start_period,omitempty"`
	Retries     int      `json:"retries,omitempty"`
	Disable     bool     `json:"disable,omitempty"`
}

// Deploy keeps only resource limits: replicas and update settings live on
// Service, so they never change the config hash.
type Deploy struct {
	Resources Resources `json:"resources"`
}

type Resources struct {
	Limits *Limits `json:"limits,omitempty"`
}

// Limits caps a replica: CPUs in cores, Memory in bytes.
type Limits struct {
	CPUs   float64 `json:"cpus,omitempty"`
	Memory Bytes   `json:"memory,omitempty"`
}

// Port is a fixed host port (Compose ports:).
type Port struct {
	HostIP    string `json:"host_ip,omitempty"`
	Target    int    `json:"target"`
	Published string `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

// Volume types.
const (
	VolumeBind   = "bind"
	VolumeVolume = "volume"
	VolumeTmpfs  = "tmpfs"
)

// Volume is one mount. Source is a host path for binds, the full volume
// name for named volumes and empty for anonymous ones.
type Volume struct {
	Type     string `json:"type"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Bind     *Bind  `json:"bind,omitempty"`
}

// Bind holds bind-mount options. A nil CreateHostPath means true, which is
// how compose-go wrote the default into state files.
type Bind struct {
	SELinux        string `json:"selinux,omitempty"`
	CreateHostPath *bool  `json:"create_host_path,omitempty"`
}

// CreatesSource reports whether a missing bind source directory is
// created, as Compose does. Binds without a bind block (long syntax) are
// not, which matches what compose-go did.
func (v Volume) CreatesSource() bool {
	return v.Type == VolumeBind && v.Bind != nil && (v.Bind.CreateHostPath == nil || *v.Bind.CreateHostPath)
}

// Duration is a time.Duration written to JSON as "5s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("duration: want a string like \"5s\", got %s", b)
		}
		*d = Duration(n)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Bytes is a size in bytes. It also reads the decimal strings compose-go
// wrote for memory limits.
type Bytes int64

func (b *Bytes) UnmarshalJSON(data []byte) error {
	n, err := strconv.ParseInt(strings.Trim(string(data), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("bytes: %w", err)
	}
	*b = Bytes(n)
	return nil
}
