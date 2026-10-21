package yamlite

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type m = map[string]any
type l = []any

func n(s string) json.Number { return json.Number(s) }

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want any
	}{
		{"empty", "", nil},
		{"comment only", "# hi\n", nil},
		{"scalar map", "a: 1\nb: x\nc: true\nd: ~\ne:\n", m{"a": n("1"), "b": "x", "c": true, "d": nil, "e": nil}},
		{"nested map", "a:\n  b:\n    c: d\n", m{"a": m{"b": m{"c": "d"}}}},
		{"sequence", "- a\n- 2\n-\n", l{"a", n("2"), nil}},
		{"sequence under key, same indent", "a:\n- x\n- y\nb: z\n", m{"a": l{"x", "y"}, "b": "z"}},
		{"sequence under key, indented", "a:\n  - x\n  - y\n", m{"a": l{"x", "y"}}},
		{"map items", "- name: a\n  port: 80\n- name: b\n", l{m{"name": "a", "port": n("80")}, m{"name": "b"}}},
		{"nested seq items", "- - a\n  - b\n- c\n", l{l{"a", "b"}, "c"}},
		{"item with nested block", "-\n  a: 1\n", l{m{"a": n("1")}}},
		{"quoted", "a: \"x: #y\\n\\u00e9\"\nb: 'it''s'\n", m{"a": "x: #y\né", "b": "it's"}},
		{"quoted key", "\"a b\": 1\n'c': 2\n", m{"a b": n("1"), "c": n("2")}},
		{"comments", "a: b # c\nd: e#f\n# g\n", m{"a": "b", "d": "e#f"}},
		{"colon in value", "image: ghcr.io/x/y:1.4\nurl: http://a:80/b\nports: 8000:8080\n", m{"image": "ghcr.io/x/y:1.4", "url": "http://a:80/b", "ports": "8000:8080"}},
		{"yaml 1.2 typing", "a: yes\nb: on\nc: 012\nd: 0x1F\ne: 0o17\nf: 1.5\ng: 1e3\nh: -3\ni: True\nj: .inf\n",
			m{"a": "yes", "b": "on", "c": "012", "d": n("31"), "e": n("15"), "f": n("1.5"), "g": n("1e3"), "h": n("-3"), "i": true, "j": ".inf"}},
		{"numbers keep their text", "big: 12345678901234567890\nv: 1.10\nz: -0\nu: 022\np: +1\n",
			m{"big": n("12345678901234567890"), "v": n("1.10"), "z": n("-0"), "u": "022", "p": "+1"}},
		{"multi-line plain", "test: curl -f http://x\n  || exit 1\nb: 1\n", m{"test": "curl -f http://x || exit 1", "b": n("1")}},
		{"multi-line plain in item", "- a\n  b\n\n  c\n- d\n", l{"a b\nc", "d"}},
		{"multi-line double quoted", "a: \"one\n  two\n\n  three\"\nb: \"x\\\n  y\"\n", m{"a": "one two\nthree", "b": "xy"}},
		{"multi-line single quoted", "a: 'one\n  two'\n", m{"a": "one two"}},
		{"tabs inside a block scalar", "run: |\n  if true; then\n  \techo hi\n  fi\n", m{"run": "if true; then\n\techo hi\nfi\n"}},
		{"more escapes", "a: \"\\x41\\U0001F600\\e\\_\\t\"\n", m{"a": "A\U0001F600\x1b\u00a0\t"}},
		{"document start comment", "--- # hi\na: 1\n", m{"a": n("1")}},
		{"flow", "a: [1, \"b\", c]\nb: {x: 1, \"y\": [2]}\nc: []\nd: {}\n", m{"a": l{n("1"), "b", "c"}, "b": m{"x": n("1"), "y": l{n("2")}}, "c": l{}, "d": m{}}},
		{"flow over lines", "a: [\n  1,  # one\n  2\n]\nb: x\n", m{"a": l{n("1"), n("2")}, "b": "x"}},
		{"json-like flow", "{\"a\":1,\"b\":[true,null]}\n", m{"a": n("1"), "b": l{true, nil}}},
		{"flow ports", "ports: [\"80:80\", 8080:80]\n", m{"ports": l{"80:80", "8080:80"}}},
		{"literal", "a: |\n  x\n   y\n\n  z\nb: 1\n", m{"a": "x\n y\n\nz\n", "b": n("1")}},
		{"literal strip keep", "a: |-\n  x\n\nb: |+\n  y\n\nc: 1\n", m{"a": "x", "b": "y\n\n", "c": n("1")}},
		{"folded", "a: >\n  x\n  y\n\n  z\n", m{"a": "x y\nz\n"}},
		{"block indent indicator", "a: |2\n    x\n  y\n", m{"a": "  x\ny\n"}},
		{"document start", "---\na: 1\n", m{"a": n("1")}},
		{"document end", "a: 1\n...\n", m{"a": n("1")}},
		{"anchor and alias", "x: &v 5\ny: *v\n", m{"x": n("5"), "y": n("5")}},
		{"anchored map", "base: &b\n  a: 1\nother: *b\n", m{"base": m{"a": n("1")}, "other": m{"a": n("1")}}},
		{"merge", "base: &b\n  a: 1\n  b: 2\nsvc:\n  <<: *b\n  b: 3\n", m{"base": m{"a": n("1"), "b": n("2")}, "svc": m{"a": n("1"), "b": n("3")}}},
		{"merge list, first wins", "x: &x {a: 1}\ny: &y {a: 2, b: 2}\nz:\n  <<: [*x, *y]\n", m{"x": m{"a": n("1")}, "y": m{"a": n("2"), "b": n("2")}, "z": m{"a": n("1"), "b": n("2")}}},
		{"alias in flow", "a: &a x\nb: [*a, y]\n", m{"a": "x", "b": l{"x", "y"}}},
		{"crlf", "a: 1\r\nb: 2\r\n", m{"a": n("1"), "b": n("2")}},
		{"tab inside value", "a: x\ty\n", m{"a": "x\ty"}},
		{"keep at end of file", "a: |+\n  y\n\n\n", m{"a": "y\n\n\n"}},
		{"clip without final newline", "a: |\n  x", m{"a": "x"}},
		{"keep without final newline", "a: |+\n  x", m{"a": "x"}},
		{"folded without final newline", "a: >\n  x", m{"a": "x"}},
		{"keep, last line spaces only", "a: |+\n  x\n  ", m{"a": "x\n"}},
		{"keep before document end", "a: |+\n  x\n...\n", m{"a": "x\n"}},
		{"empty block scalars (spec 8.6)", "strip: >-\n\nclip: >\n\nkeep: |+\n\n", m{"strip": "", "clip": "", "keep": "\n"}},
		{"empty keep at end of file", "a: |+\n", m{"a": ""}},
		{"merge in flow", "a: &a {x: 1, y: 1}\nb: {<<: *a, y: 2}\n", m{"a": m{"x": n("1"), "y": n("1")}, "b": m{"x": n("1"), "y": n("2")}}},
		{"merge list in flow", "a: &a {x: 1}\nb: &b {x: 2, z: 2}\nc: {y: 3, <<: [*a, *b]}\n", m{"a": m{"x": n("1")}, "b": m{"x": n("2"), "z": n("2")}, "c": m{"x": n("1"), "y": n("3"), "z": n("2")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"tab indent", "a:\n\tb: 1\n", "2:1: tabs are not allowed for indentation"},
		{"two documents", "a: 1\n---\nb: 2\n", "2:1: multiple documents are not supported"},
		{"tag", "a: !!str 1\n", "1:4: tags are not supported"},
		{"custom tag", "a: !reset []\n", "1:4: tags are not supported"},
		{"complex key", "? a\n: b\n", "1:1: complex keys are not supported"},
		{"directive", "%YAML 1.2\n---\na: 1\n", "1:1: directives are not supported"},
		{"duplicate key", "a: 1\na: 2\n", "2:1: duplicate key \"a\""},
		{"undefined alias", "a: *nope\n", "1:4: undefined alias *nope"},
		{"self alias", "a: &x [*x]\n", "undefined alias *x"},
		{"bad indent", "a: 1\n  b: 2\n", "2:3: bad indentation"},
		{"tab indent after block", "a: |\n  x\n\tb: 1\n", "3:1: tabs are not allowed for indentation"},
		{"unterminated quote", "a: \"x\n", "1:4: unterminated quoted string"},
		{"unterminated flow", "a: [1, 2\n", "1:4: unterminated flow collection"},
		{"bad merge", "a:\n  <<: 1\n", "2:3: << needs a mapping or a list of mappings"},
		{"seq in map", "a: 1\n- b\n", "2:1: expected a key"},
		{"text after quote", "a: \"x\" y\n", "1:8: unexpected text after quoted string"},
		{"bad merge in flow", "a: {<<: 1}\n", "1:5: << needs a mapping or a list of mappings"},
		{"duplicate key in multi-line flow", "a: {\n  b: 1,\n  b: 2\n}\n", "3:3: duplicate key \"b\""},
		{"unterminated quote in multi-line flow", "a: [\n  1,\n  \"x\n]\n", "3:3: unterminated quoted string"},
		{"bad escape in multi-line flow", "a: [\n  1,\n  \"\\q\"\n]\n", "3:3: unknown escape"},
		{"missing comma in multi-line flow", "a: [\n  1\n  2\n]\n", "3:3: expected , or ]"},
		{"text after multi-line flow", "a: [1,\n 2] x\n", "2:5: unexpected text after flow collection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"a: 1\n", "- a\n- b: [1, {c: d}]\n", "a: |\n  x\n", "x: &a {b: 1}\ny:\n  <<: *a\n",
		"a: \"\\u00e9\"\n", "a: [\n1,\n", "? a\n", "\t- a\n", "a: >-\n  b\n\n  c\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		if _, err := json.Marshal(v); err != nil {
			t.Fatalf("tree does not marshal: %v", err)
		}
	})
}
