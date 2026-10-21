package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rationislabs/anastasis/internal/model"
)

func TestExpand(t *testing.T) {
	vars := map[string]string{"A": "a", "E": ""}
	tests := []struct{ in, want string }{
		{"$A", "a"}, {"${A}", "a"}, {"x${A}y", "xay"}, {"$$A", "$A"}, {"$", "$"}, {"a$-b", "a$-b"},
		{"${U:-d}", "d"}, {"${E:-d}", "d"}, {"${E-d}", ""}, {"${U-d}", "d"},
		{"${A:+alt}", "alt"}, {"${E:+alt}", ""}, {"${E+alt}", "alt"}, {"${U+alt}", ""},
		{"${U:-${A}}", "a"}, {"${U:-two words}", "two words"}, {"$U", ""},
	}
	for _, tt := range tests {
		if got, err := expand(tt.in, vars); err != nil || got != tt.want {
			t.Errorf("expand(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for in, want := range map[string]string{
		"${U:?need U}": "need U", "${E:?}": "E", "${U?}": "U", "${A": "missing }", "${}": "bad substitution", "${A:x}": "bad substitution",
	} {
		if _, err := expand(in, vars); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expand(%q) err = %v, want %q", in, err, want)
		}
	}
}

func TestInterpolateCopies(t *testing.T) {
	shared := map[string]any{"a": "$$x"}
	tree := map[string]any{"one": shared, "two": shared}
	left := maxValues
	out, err := interpolate(tree, nil, "", &left)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["one"].(map[string]any)["a"] != "$x" || m["two"].(map[string]any)["a"] != "$x" || shared["a"] != "$$x" {
		t.Fatalf("out %v shared %v: a shared (aliased) value must be interpolated once per use, not in place", out, shared)
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	os.WriteFile(path, []byte("# c\n\nexport A=1\nB = \"x\\ny\"\nC='$z'\nD=v # note\nE=a#b\nFROM\nMISSING\n"), 0o600)
	got, err := readEnvFile(path, map[string]string{"FROM": "env"})
	want := map[string]string{"A": "1", "B": "x\ny", "C": "$z", "D": "v", "E": "a#b", "FROM": "env"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	os.WriteFile(path, []byte("HOST=db\nURL=http://${HOST}:5432\nQ=\"x ${HOST}\"\nA=\"x\" # c\nB='y' # c\nE=\"multi\nline\"\nF=\"tab\\there\"\nS='${HOST}'\nO=${FROM}-x\n"), 0o600)
	got, err = readEnvFile(path, map[string]string{"FROM": "env"})
	want = map[string]string{"HOST": "db", "URL": "http://db:5432", "Q": "x db", "A": "x", "B": "y", "E": "multi\nline", "F": "tab\there", "S": "${HOST}", "O": "env-x"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	os.WriteFile(path, []byte("=x\n"), 0o600)
	if _, err := readEnvFile(path, nil); err == nil || !strings.Contains(err.Error(), "x.env:1") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitShell(t *testing.T) {
	tests := map[string][]string{
		"serve":                     {"serve"},
		"nginx -g 'daemon off;'":    {"nginx", "-g", "daemon off;"},
		`sh -c "echo \"hi\" $HOME"`: {"sh", "-c", `echo "hi" $HOME`},
		`a\ b  c`:                   {"a b", "c"},
		`''`:                        {""},
	}
	for in, want := range tests {
		if got, err := splitShell(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitShell(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitShell(`echo "x`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestParsePort(t *testing.T) {
	tests := map[string]string{
		"80":                    "{HostIP: Target:80 Published: Protocol:tcp}",
		"8080:80":               "{HostIP: Target:80 Published:8080 Protocol:tcp}",
		"127.0.0.1:8080:80/udp": "{HostIP:127.0.0.1 Target:80 Published:8080 Protocol:udp}",
		"127.0.0.1::80":         "{HostIP:127.0.0.1 Target:80 Published: Protocol:tcp}",
		"[::1]:8080:80":         "{HostIP:::1 Target:80 Published:8080 Protocol:tcp}",
	}
	for in, want := range tests {
		p, err := parsePort(in)
		if got := strings.TrimPrefix(fmtPort(p), "model.Port"); err != nil || got != want {
			t.Errorf("parsePort(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"8000-8001:80", "x:80", "70000", "80:0", ""} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("parsePort(%q) accepted", bad)
		}
	}
}

func TestParseVolume(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := map[string]string{
		"/data":                "{Type:volume Source: Target:/data ReadOnly:false Bind:<nil>}",
		"data:/data":           "{Type:volume Source:data Target:/data ReadOnly:false Bind:<nil>}",
		"/data:ro":             "{Type:volume Source: Target:/data ReadOnly:true Bind:<nil>}",
		"./c:/c/:ro,z":         "{Type:bind Source:/proj/c Target:/c ReadOnly:true Bind:&{SELinux:z CreateHostPath:<nil>}}",
		"../c:/c":              "{Type:bind Source:/c Target:/c ReadOnly:false Bind:&{SELinux: CreateHostPath:<nil>}}",
		"~/x:/x":               "{Type:bind Source:" + home + "/x Target:/x ReadOnly:false Bind:&{SELinux: CreateHostPath:<nil>}}",
		"/srv/a:/a:rw":         "{Type:bind Source:/srv/a Target:/a ReadOnly:false Bind:&{SELinux: CreateHostPath:<nil>}}",
		"./src:/app:cached":    "{Type:bind Source:/proj/src Target:/app ReadOnly:false Bind:&{SELinux: CreateHostPath:<nil>}}",
		"data:/d:ro,delegated": "{Type:volume Source:data Target:/d ReadOnly:true Bind:<nil>}",
	}
	for in, want := range tests {
		v, err := parseVolume(in, "/proj")
		if got := fmtVolume(v); err != nil || got != want {
			t.Errorf("parseVolume(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"data:/d:nocopy": "nocopy", "/a:/b:rshared": "propagation", "data:/d:z": "bind mounts only", "a:b": "absolute", "a:/b:c:d": "want",
	} {
		if _, err := parseVolume(in, "/proj"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseVolume(%q) err = %v, want %q", in, err, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1024": 1024, "256m": 256 << 20, "256M": 256 << 20, "1g": 1 << 30, "1GB": 1 << 30, "512MiB": 512 << 20, "1.5k": 1536, "10b": 10} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "m", "1x", "-1"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}

func fmtPort(p model.Port) string { return fmt.Sprintf("%+v", p) }

func fmtVolume(v model.Volume) string {
	s := fmt.Sprintf("%+v", v)
	if v.Bind != nil {
		s = strings.Replace(s, fmt.Sprintf("%p", v.Bind), fmt.Sprintf("&%+v", *v.Bind), 1)
	}
	return s
}
