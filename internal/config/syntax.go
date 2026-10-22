package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/rationislabs/anastasis/internal/model"
)

// maxValues and maxBytes cap what interpolate copies. The parsed tree shares
// an aliased value, but the copy expands every use, so nested aliases (a
// "billion laughs" file) would grow exponentially: in values, or, with few
// values, in bytes (one 1 MB string used 10,000 times).
const (
	maxValues = 100_000
	maxBytes  = 64 << 20
)

var errTooBig = fmt.Errorf("values expand to more than %d MiB; check for nested aliases or variables", maxBytes>>20)

// budget is what interpolate may still copy.
type budget struct{ values, bytes int }

func newBudget() *budget { return &budget{maxValues, maxBytes} }

// interpolate returns a copy of v with variables expanded in every string.
// It copies instead of editing in place: YAML aliases share one value, and
// expanding a shared string twice would turn $$ into $ and then expand it.
func interpolate(v any, vars map[string]string, at string, left *budget) (any, error) {
	if left.values--; left.values < 0 {
		return nil, fmt.Errorf("more than %d values once aliases are expanded; check for nested YAML aliases", maxValues)
	}
	switch t := v.(type) {
	case string:
		s, err := expand(t, vars)
		if err != nil {
			if at == "" {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		if left.bytes -= len(s); left.bytes < 0 { // what it expands to, not what it says
			return nil, errTooBig
		}
		return s, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			p := k
			if at != "" {
				p = at + "." + k
			}
			r, err := interpolate(e, vars, p, left)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := interpolate(e, vars, fmt.Sprintf("%s[%d]", at, i), left)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func isNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isNameChar(c byte) bool { return isNameStart(c) || c >= '0' && c <= '9' }

// expand replaces $VAR, ${VAR}, ${VAR:-default}, ${VAR-default},
// ${VAR:?message}, ${VAR?message}, ${VAR:+alt} and ${VAR+alt}, and $$
// with $. An unset variable without a default expands to "", as in
// compose-go. Defaults and alternatives may contain ${...} themselves.
func expand(s string, vars map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch c := s[i+1]; {
		case c == '$':
			b.WriteByte('$')
			i++
		case c == '{':
			end := closing(s, i+2)
			if end < 0 {
				return "", fmt.Errorf("missing } in %q", s)
			}
			r, err := substitute(s[i+2:end], vars)
			if err != nil {
				return "", err
			}
			b.WriteString(r)
			i = end
			if b.Len() > maxBytes { // variables naming variables: a few bytes can name gigabytes
				return "", errTooBig
			}
		case isNameStart(c):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			b.WriteString(vars[s[i+1:j]])
			i = j - 1
			if b.Len() > maxBytes {
				return "", errTooBig
			}
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// closing finds the } that closes a ${ whose body starts at from.
func closing(s string, from int) int {
	depth := 1
	for j := from; j < len(s); j++ {
		switch s[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

func substitute(expr string, vars map[string]string) (string, error) {
	j := 0
	for j < len(expr) && isNameChar(expr[j]) {
		j++
	}
	name, op := expr[:j], expr[j:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("bad substitution ${%s}", expr)
	}
	val, set := vars[name]
	required := func(msg string) (string, error) {
		if msg == "" {
			msg = "is required"
		}
		return "", fmt.Errorf("variable %s: %s", name, msg)
	}
	switch {
	case op == "":
		return val, nil
	case strings.HasPrefix(op, ":-"):
		if val == "" {
			return expand(op[2:], vars)
		}
	case strings.HasPrefix(op, "-"):
		if !set {
			return expand(op[1:], vars)
		}
	case strings.HasPrefix(op, ":?"):
		if val == "" {
			return required(op[2:])
		}
	case strings.HasPrefix(op, "?"):
		if !set {
			return required(op[1:])
		}
	case strings.HasPrefix(op, ":+"):
		if val == "" {
			return "", nil
		}
		return expand(op[2:], vars)
	case strings.HasPrefix(op, "+"):
		if !set {
			return "", nil
		}
		return expand(op[1:], vars)
	default:
		return "", fmt.Errorf("bad substitution ${%s}", expr)
	}
	return val, nil
}

// readEnvFile parses KEY=VALUE lines like Compose: blank lines and #
// comments are skipped, "export " is allowed, and a bare KEY takes its value
// from vars (or is left out). Unquoted and double-quoted values expand
// ${VAR} from earlier lines and vars; single-quoted values are literal.
// Quoted values may span lines and may be followed by a # comment.
func readEnvFile(file string, vars map[string]string) (map[string]string, error) {
	data, err := readCapped(file)
	if err != nil {
		return nil, err
	}
	scope := maps.Clone(vars)
	if scope == nil {
		scope = map[string]string{}
	}
	out := map[string]string{}
	total := 0 // bytes of values so far: they can name each other
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for n := 0; n < len(lines); n++ {
		at := n + 1
		ln := strings.TrimSpace(lines[n])
		if ln == "" || ln[0] == '#' {
			continue
		}
		ln = strings.TrimPrefix(ln, "export ")
		k, v, ok := strings.Cut(ln, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", file, at)
		}
		if !ok {
			if val, set := vars[k]; set {
				out[k] = val
			}
			continue
		}
		v = strings.TrimLeft(v, " \t")
		if v != "" && (v[0] == '"' || v[0] == '\'') {
			// Each line is scanned once (an escape never spans a line break) and the
			// value joined once: re-scanning a growing body made it quadratic.
			q, parts := v[0], []string{v[1:]}
			end := closingQuote(parts[0], q)
			for end < 0 {
				if n++; n == len(lines) {
					return nil, fmt.Errorf("%s:%d: unterminated quoted value", file, at)
				}
				parts = append(parts, lines[n])
				end = closingQuote(lines[n], q)
			}
			last := parts[len(parts)-1]
			if rest := strings.TrimSpace(last[end+1:]); rest != "" && rest[0] != '#' {
				return nil, fmt.Errorf("%s:%d: unexpected text after the closing quote", file, at)
			}
			parts[len(parts)-1] = last[:end]
			body := strings.Join(parts, "\n")
			if q == '\'' {
				out[k], scope[k] = body, body
				continue
			}
			v = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`).Replace(body)
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		} else {
			v = strings.TrimSpace(v)
		}
		v, err := expand(v, scope)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, at, err)
		}
		if total += len(v); total > maxBytes {
			return nil, fmt.Errorf("%s:%d: %w", file, at, errTooBig)
		}
		out[k], scope[k] = v, v
	}
	return out, nil
}

// closingQuote finds the quote that ends a quoted value; in double quotes a
// backslash escapes the next character.
func closingQuote(s string, q byte) int {
	for i := 0; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

// splitShell splits a command string into words like a POSIX shell, without
// expanding anything: quotes group, a backslash escapes the next character.
func splitShell(s string) ([]string, error) {
	var words []string
	var b strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, b.String())
				b.Reset()
				inWord = false
			}
		case c == '\\':
			inWord = true
			if i++; i < len(s) {
				b.WriteByte(s[i])
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated ' in command")
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New(`unterminated " in command`)
			}
		default:
			inWord = true
			b.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, b.String())
	}
	return words, nil
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// parsePort reads "[[host_ip:]published:]target[/protocol]".
func parsePort(s string) (model.Port, error) {
	p := model.Port{Protocol: "tcp"}
	spec := s
	if before, proto, ok := strings.Cut(s, "/"); ok {
		var err error
		if p.Protocol, err = protocol(proto); err != nil {
			return p, err
		}
		spec = before
	}
	if strings.Contains(spec, "-") {
		return p, errors.New("port ranges are not supported")
	}
	host, target := "", spec
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		host, target = spec[:i], spec[i+1:]
	}
	if !validPort(target) {
		return p, fmt.Errorf("want [[host_ip:]published:]target, got %q", s)
	}
	p.Target, _ = strconv.Atoi(target)
	pub := host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		p.HostIP, pub = strings.Trim(host[:i], "[]"), host[i+1:]
	}
	if pub != "" && !validPort(pub) {
		return p, fmt.Errorf("want [[host_ip:]published:]target, got %q", s)
	}
	p.Published = pub
	return p, nil
}

// protocol lowercases a port protocol and accepts the ones Podman publishes.
func protocol(s string) (string, error) {
	p := strings.ToLower(s)
	if p != "tcp" && p != "udp" && p != "sctp" {
		return "", fmt.Errorf("protocol must be tcp, udp or sctp, got %q", s)
	}
	return p, nil
}

func isHostPath(s string) bool {
	return strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~")
}

// resolvePath makes a host path absolute: ~ is the home directory, relative
// paths are relative to the project file's directory.
func resolvePath(p, dir string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// parseVolume reads "[source:]target[:mode]". A source that looks like a
// path (./, /, ~) is a bind mount; any other source is a named volume; no
// source is an anonymous volume.
func parseVolume(s, dir string) (model.Volume, error) {
	parts := strings.Split(s, ":")
	var src, target, mode string
	switch {
	case len(parts) == 1:
		target = parts[0]
	case len(parts) == 2 && strings.HasPrefix(parts[1], "/"):
		src, target = parts[0], parts[1]
	case len(parts) == 2:
		target, mode = parts[0], parts[1]
	case len(parts) == 3:
		src, target, mode = parts[0], parts[1], parts[2]
	default:
		return model.Volume{}, fmt.Errorf("want [source:]target[:mode], got %q", s)
	}
	if !strings.HasPrefix(target, "/") {
		return model.Volume{}, fmt.Errorf("target must be an absolute path in the container, got %q", target)
	}
	v := model.Volume{Type: model.VolumeVolume, Target: path.Clean(target)}
	switch {
	case src == "":
	case isHostPath(src):
		v.Type, v.Source, v.Bind = model.VolumeBind, resolvePath(src, dir), &model.Bind{}
	default:
		v.Source = src
	}
	for _, o := range strings.Split(mode, ",") {
		switch o {
		case "", "rw":
		case "ro":
			v.ReadOnly = true
		case "z", "Z":
			if v.Bind == nil {
				return v, fmt.Errorf("%s: z and Z apply to bind mounts only", s)
			}
			v.Bind.SELinux = o
		case "cached", "delegated", "consistent":
			// macOS file-sharing hints; nothing to do on Linux.
		case "nocopy":
			return v, fmt.Errorf("%s: nocopy and subpath are not supported", s)
		case "private", "rprivate", "shared", "rshared", "slave", "rslave":
			return v, fmt.Errorf("%s: bind propagation and recursive are not supported", s)
		default:
			return v, fmt.Errorf("%s: unknown mode %q", s, o)
		}
	}
	return v, nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?)(?:i?b)?$`)

// parseSize reads a byte count: a plain number, or a number with a k, m, g,
// t or p suffix (binary multiples, optional "b" or "ib"), as Compose does.
func parseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("want a size like 256m or 1g, got %q", s)
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	shift := map[string]uint{"": 0, "k": 10, "m": 20, "g": 30, "t": 40, "p": 50}[m[2]]
	return int64(f * float64(uint64(1)<<shift)), nil
}
