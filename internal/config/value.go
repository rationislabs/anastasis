package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

// at is a value in the project tree with its key path, so every error says
// where it is ("deploy.replicas: want a whole number"). Strings are
// accepted where numbers or booleans are expected, because interpolation
// (replicas: ${N}) always produces strings.
type at struct {
	path string
	v    any
}

func (a at) key(k string) at {
	m, _ := a.v.(map[string]any)
	if a.path == "" {
		return at{k, m[k]}
	}
	return at{a.path + "." + k, m[k]}
}

func (a at) index(i int, v any) at { return at{fmt.Sprintf("%s[%d]", a.path, i), v} }

func (a at) has(k string) bool {
	m, _ := a.v.(map[string]any)
	_, ok := m[k]
	return ok
}

func (a at) errorf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if a.path == "" {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("%s: %s", a.path, msg)
}

func kind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	}
	return "nothing"
}

func (a at) mapping() (map[string]any, error) {
	switch m := a.v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return m, nil
	}
	return nil, a.errorf("want a mapping, got %s", kind(a.v))
}

func (a at) list() ([]any, error) {
	switch l := a.v.(type) {
	case nil:
		return nil, nil
	case []any:
		return l, nil
	}
	return nil, a.errorf("want a list, got %s", kind(a.v))
}

// only rejects keys outside allowed.
func (a at) only(allowed ...string) error {
	m, err := a.mapping()
	if err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, k) {
			return a.key(k).errorf("is not supported by anastasis")
		}
	}
	return nil
}

func (a at) str() (string, error) {
	switch v := a.v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", a.errorf("want a string, got %s", kind(a.v))
}

func (a at) strs() ([]string, error) {
	l, err := a.list()
	if err != nil {
		return nil, err
	}
	var out []string
	for i, e := range l {
		s, err := a.index(i, e).str()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// integer returns def when the key is absent or null.
func (a at) integer(def int) (int, error) {
	switch v := a.v.(type) {
	case nil:
		return def, nil
	case json.Number, string:
		if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil {
			return n, nil
		}
	}
	return 0, a.errorf("want a whole number, got %v", a.v)
}

func (a at) number() (float64, error) {
	switch v := a.v.(type) {
	case nil:
		return 0, nil
	case json.Number, string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64); err == nil {
			return f, nil
		}
	}
	return 0, a.errorf("want a number, got %v", a.v)
}

func (a at) boolean() (bool, error) {
	switch v := a.v.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		// The spellings compose-go accepts; YAML 1.2 leaves yes/no/on/off as strings.
		switch strings.ToLower(v) {
		case "true", "yes", "y", "on":
			return true, nil
		case "false", "no", "n", "off":
			return false, nil
		}
	}
	return false, a.errorf("want true or false, got %v", a.v)
}

func (a at) duration() (model.Duration, error) {
	switch v := a.v.(type) {
	case nil:
		return 0, nil
	case string:
		d, err := parseDuration(v)
		if errors.Is(err, errTooLong) {
			return 0, a.errorf("a duration too long: it must be under %d days, got %q", maxDays, v)
		}
		if err != nil {
			return 0, a.errorf("want a duration like 5s, 1m30s or 1d, got %q", v)
		}
		if d < 0 {
			return 0, a.errorf("a duration must not be negative, got %q", v)
		}
		return model.Duration(d), nil
	}
	return 0, a.errorf("want a duration like 5s, 1m30s or 1d, got %v", a.v)
}

var weeksDays = regexp.MustCompile(`^(?:([0-9]+)w)?(?:([0-9]+)d)?`)

// maxDays is the longest duration an int64 of nanoseconds holds, in whole days.
const maxDays = math.MaxInt64 / int64(24*time.Hour)

var errTooLong = errors.New("duration too long")

// goDuration is time.ParseDuration's syntax: when it matches and parsing still
// fails, the number did not fit.
var goDuration = regexp.MustCompile(`^[-+]?(?:(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:ns|us|µs|μs|ms|s|m|h))+$`)

// parseDuration is time.ParseDuration plus leading w (7 days) and d (24h)
// units, as Compose accepts: 1w2d3h.
// ponytail: whole weeks and days only, in that order; 1.5d fails.
func parseDuration(s string) (time.Duration, error) {
	m := weeksDays.FindStringSubmatch(s)
	var d time.Duration
	if rest := s[len(m[0]):]; rest != "" || m[0] == "" {
		var err error
		if d, err = time.ParseDuration(rest); err != nil {
			if goDuration.MatchString(rest) {
				return 0, errTooLong
			}
			return 0, err
		}
	}
	var days int64
	for i, per := range []int64{7, 1} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > maxDays/per || days+n*per > maxDays {
			return 0, errTooLong
		}
		days += n * per
	}
	whole := time.Duration(days) * 24 * time.Hour
	if d > 0 && whole > math.MaxInt64-d {
		return 0, errTooLong
	}
	return d + whole, nil
}

// size reads a byte count: a number, or a string like "256m".
func (a at) size() (int64, error) {
	switch v := a.v.(type) {
	case nil:
		return 0, nil
	case json.Number, string:
		if n, err := parseSize(fmt.Sprint(v)); err == nil {
			return n, nil
		}
	}
	return 0, a.errorf("want a size like 256m or 1g, got %v", a.v)
}

// command reads a list, or a string split into words like a shell would.
// Set but empty ([] or "") gives an empty, non-nil slice: it clears the
// image's default, where nil keeps it.
func (a at) command() ([]string, error) {
	var w []string
	var err error
	if s, ok := a.v.(string); ok {
		if w, err = splitShell(s); err != nil {
			return nil, a.errorf("%v", err)
		}
	} else if w, err = a.strs(); err != nil {
		return nil, err
	}
	if w == nil && a.v != nil {
		w = []string{}
	}
	return w, nil
}

// pairs reads a KEY=VALUE list or a mapping. A bare KEY, or KEY: null, has
// a nil value.
func (a at) pairs() (map[string]*string, error) {
	out := map[string]*string{}
	switch v := a.v.(type) {
	case nil:
	case []any:
		items, err := a.strs()
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if k, val, ok := strings.Cut(it, "="); ok {
				out[k] = &val
			} else {
				out[it] = nil
			}
		}
	case map[string]any:
		for k, e := range v {
			if e == nil {
				out[k] = nil
				continue
			}
			s, err := a.key(k).str()
			if err != nil {
				return nil, err
			}
			out[k] = &s
		}
	default:
		return nil, a.errorf("want a list or a mapping, got %s", kind(a.v))
	}
	return out, nil
}
