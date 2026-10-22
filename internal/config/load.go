// Package config loads Compose-shaped project files, JSON or YAML, into
// anastasis's model and rejects what anastasis cannot run, so a file never
// silently runs differently than written.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/yamlite"
)

// maxFileBytes caps a project file: a compose file is a few KB, and the parser
// holds all of it in memory.
const maxFileBytes = 4 << 20

// readCapped reads path, refusing a file over maxFileBytes before parsing it.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s: larger than %d MiB; is it a compose file?", path, maxFileBytes>>20)
	}
	return data, nil
}

// Load reads the project file at path. env ("KEY=value") and the .env file
// next to it supply ${VAR} values; env wins. Files ending in .json are read
// as JSON, anything else as YAML. ctx is unused: loading reads local files
// only.
func Load(ctx context.Context, path string, env []string) (*model.Project, error) {
	data, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	tree, err := parse(path, data)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	vars, err := variables(dir, env)
	if err != nil {
		return nil, err
	}
	name, err := projectName(tree, dir, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	vars["COMPOSE_PROJECT_NAME"] = name
	if tree, err = interpolate(tree, vars, "", newBudget()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p, err := project(at{v: tree}, name, dir, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func parse(path string, data []byte) (any, error) {
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		v, err := yamlite.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s:%w", path, err)
		}
		return v, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	j := &jsonTree{dec: dec, data: data}
	v, err := j.value()
	if err == nil {
		after := dec.InputOffset()
		if _, err2 := dec.Token(); err2 != io.EOF {
			err, j.off = errors.New("unexpected data after the top-level value"), skipSpace(data, after)
		}
	}
	if err == nil {
		return v, nil
	}
	end := int64(len(bytes.TrimRight(data, " \t\r\n")))
	var se *json.SyntaxError
	switch {
	case errors.As(err, &se):
		j.off = se.Offset
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		err, j.off = errors.New("unexpected end of JSON input"), end
	}
	line, col := position(data, min(j.off, end)) // an end-of-input error points just past the last token
	return nil, fmt.Errorf("%s:%d:%d: %v", path, line, col, err)
}

// jsonTree builds the same tree as Decode with UseNumber, token by token,
// so it can reject duplicate keys with their position. off is where an
// error it found itself is.
type jsonTree struct {
	dec  *json.Decoder
	data []byte
	off  int64
}

func (j *jsonTree) value() (any, error) {
	t, err := j.dec.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for j.dec.More() {
			at := skipSpace(j.data, j.dec.InputOffset())
			k, err := j.dec.Token()
			if err != nil {
				return nil, err
			}
			key := k.(string) // the decoder rejects any other key
			if _, dup := m[key]; dup {
				j.off = at
				return nil, fmt.Errorf("duplicate key %q", key)
			}
			if m[key], err = j.value(); err != nil {
				return nil, err
			}
		}
		_, err = j.dec.Token()
		return m, err
	case json.Delim('['):
		l := []any{}
		for j.dec.More() {
			e, err := j.value()
			if err != nil {
				return nil, err
			}
			l = append(l, e)
		}
		_, err = j.dec.Token()
		return l, err
	}
	return t, nil
}

// skipSpace moves off past blanks and a comma, to the start of the next token.
func skipSpace(data []byte, off int64) int64 {
	rest := data[off:]
	return off + int64(len(rest)-len(bytes.TrimLeft(rest, " \t\r\n,")))
}

// position turns a byte offset into a 1-based line and column.
func position(data []byte, off int64) (int, int) {
	before := data[:min(int(off), len(data))]
	line := bytes.Count(before, []byte("\n")) + 1
	return line, len(before) - bytes.LastIndexByte(before, '\n')
}

func variables(dir string, env []string) (map[string]string, error) {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	dot := filepath.Join(dir, ".env")
	if _, err := os.Stat(dot); err == nil {
		m, err := readEnvFile(dot, vars)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			if _, set := vars[k]; !set {
				vars[k] = v // the process environment wins
			}
		}
	}
	return vars, nil
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// serviceName is what Compose accepts; a name becomes part of container names
// (after the project's), URL paths and the sticky cookie's name, so a name of
// dots only (".", "..") is refused as well.
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// volumeName is the engine's rule: a volume name is one path segment of its API,
// used as it is.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// projectName picks the name the way compose-go does: COMPOSE_PROJECT_NAME
// (which must already be valid), else the file's name: (interpolated and
// normalized), else the directory name, normalized.
func projectName(tree any, dir string, vars map[string]string) (string, error) {
	if name := vars["COMPOSE_PROJECT_NAME"]; name != "" {
		if !validName.MatchString(name) {
			return "", fmt.Errorf("invalid project name %q: use lowercase letters, digits, - and _, starting with a letter or digit", name)
		}
		return name, nil
	}
	if m, ok := tree.(map[string]any); ok {
		if s, ok := m["name"].(string); ok {
			name, err := expand(s, vars)
			if err != nil {
				return "", fmt.Errorf("name: %w", err)
			}
			if name = normalizeName(name); name != "" {
				return name, nil
			}
		}
	}
	if name := normalizeName(filepath.Base(dir)); name != "" {
		return name, nil
	}
	return "", errors.New("cannot derive a project name from the directory; set the top-level name")
}

// normalizeName lowercases s and drops characters outside [a-z0-9_-] and
// leading - and _.
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

func project(root at, name, dir string, vars map[string]string) (*model.Project, error) {
	top, err := root.mapping()
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, errors.New("empty project file")
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		switch k {
		case "name", "version", "services", "volumes", "networks":
		default:
			if !strings.HasPrefix(k, "x-") {
				return nil, root.key(k).errorf("is not supported by anastasis")
			}
		}
	}
	out := &model.Project{Name: name, Services: map[string]model.Service{}}
	vols, owned, err := volumes(root.key("volumes"), name)
	if err != nil {
		return nil, err
	}
	out.Volumes = owned
	if _, err := root.key("networks").mapping(); err != nil {
		return nil, err
	}
	svcs, err := root.key("services").mapping()
	if err != nil {
		return nil, err
	}
	exposed := map[int]string{}
	for _, sn := range slices.Sorted(maps.Keys(svcs)) {
		if !serviceName.MatchString(sn) || strings.Trim(sn, ".") == "" {
			return nil, fmt.Errorf("service %q: name must match %s", sn, serviceName)
		}
		svc, err := toService(name, sn, at{v: svcs[sn]}, dir, vars, vols)
		if err == nil {
			err = checkDependsOn(at{v: svcs[sn]}.key("depends_on"), svcs)
		}
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", sn, err)
		}
		if svc.Expose != nil {
			if other, dup := exposed[svc.Expose.HostPort]; dup {
				return nil, fmt.Errorf("services %q and %q both expose host port %d", other, sn, svc.Expose.HostPort)
			}
			exposed[svc.Expose.HostPort] = sn
		}
		out.Services[sn] = svc
	}
	return out, nil
}

// volumes maps each declared volume to its full name and lists the ones the
// project owns. External volumes are never owned: down --volumes must not
// delete them.
func volumes(a at, project string) (map[string]string, []string, error) {
	m, err := a.mapping()
	if err != nil {
		return nil, nil, err
	}
	full := map[string]string{}
	var owned []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := a.key(k)
		if err := v.only("name", "external", "driver", "labels"); err != nil {
			return nil, nil, err
		}
		driver, err := v.key("driver").str()
		if err != nil {
			return nil, nil, err
		}
		if driver != "" && driver != "local" {
			return nil, nil, v.key("driver").errorf("%s is not supported; anastasis uses the runtime's local volumes", driver)
		}
		ext, err := v.key("external").boolean()
		if err != nil {
			return nil, nil, err
		}
		name, err := v.key("name").str()
		if err != nil {
			return nil, nil, err
		}
		switch {
		case name != "":
		case ext:
			name = k
		default:
			name = project + "_" + k
		}
		if !volumeName.MatchString(name) { // one path segment in the engine's API
			return nil, nil, fmt.Errorf("volume %q: name must match %s", k, volumeName)
		}
		full[k] = name
		if !ext {
			owned = append(owned, name)
		}
	}
	slices.Sort(owned)
	return full, owned, nil
}

func parseExpose(s string) (model.Expose, error) {
	h, c, ok := strings.Cut(s, ":")
	if !ok {
		return model.Expose{}, fmt.Errorf("want \"hostPort:containerPort\", got %q", s)
	}
	hp, err1 := strconv.Atoi(h)
	cp, err2 := strconv.Atoi(c)
	if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || cp < 1 || cp > 65535 {
		return model.Expose{}, fmt.Errorf("ports must be 1-65535, got %q", s)
	}
	return model.Expose{HostPort: hp, ContainerPort: cp}, nil
}
