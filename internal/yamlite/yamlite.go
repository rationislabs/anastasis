// Package yamlite reads the subset of YAML that Compose files use. It
// produces the same tree as encoding/json with UseNumber: map[string]any,
// []any, string, bool, json.Number and nil, so callers handle YAML and
// JSON the same way. It follows the YAML 1.2 core schema: yes/no/on/off
// stay strings, and 8080:80 is a string, not a base-60 number.
//
// Supported: block mappings and sequences, plain and quoted scalars, flow
// collections, literal and folded block scalars, comments, anchors,
// aliases and << merge keys. Rejected with a position: tabs used for
// indentation, more than one document, tags, complex keys, directives and
// duplicate keys.
package yamlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error is a parse error at a 1-based line and column.
type Error struct {
	Line, Col int
	Msg       string
}

func (e *Error) Error() string { return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg) }

type line struct {
	num    int    // 1-based line number
	indent int    // column where text starts; moved right for "- key: v" items
	text   string // content from indent on
	raw    string // the whole line, for block scalars
	tab    bool   // indented with a tab: fine inside a block scalar, an error anywhere else
}

// maxDepth caps nesting, block and flow alike: the parser recurses per level,
// and running out of stack is a fatal error recover cannot catch. Real compose
// files nest a handful of levels.
const maxDepth = 1000

const errDeep = "nesting deeper than 1000 levels"

type parser struct {
	lines   []line
	i       int
	anchors map[string]any
	open    bool // the last line ends the file without a line break
	depth   int  // block nodes open right now
}

// Parse reads one YAML document.
func Parse(data []byte) (any, error) {
	p := &parser{anchors: map[string]any{}}
	if err := p.split(string(data)); err != nil {
		return nil, err
	}
	if err := p.skipBlank(); err != nil {
		return nil, err
	}
	if p.i == len(p.lines) {
		return nil, nil
	}
	v, err := p.node(p.lines[p.i].indent)
	if err != nil {
		return nil, err
	}
	if err := p.skipBlank(); err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		return nil, p.errAt(p.lines[p.i], p.lines[p.i].indent, "bad indentation")
	}
	return v, nil
}

func (p *parser) errAt(l line, col int, msg string) error {
	return &Error{Line: l.num, Col: col + 1, Msg: msg}
}

// split cuts the input into lines and handles document markers and
// directives, which are only allowed at column 0.
func (p *parser) split(s string) error {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	started, ended := false, false
	raws := strings.Split(s, "\n")
	for n, raw := range raws {
		text := strings.TrimLeft(raw, " ")
		trimmed := strings.TrimSpace(text)
		at := line{num: n + 1, indent: len(raw) - len(text), text: text, raw: raw}
		at.tab = trimmed != "" && text[0] == '\t'

		if at.indent == 0 {
			switch {
			case trimmed == "---" || strings.HasPrefix(trimmed, "--- "):
				if started {
					return p.errAt(at, 0, "multiple documents are not supported")
				}
				if r := strings.TrimSpace(trimmed[3:]); r != "" && r[0] != '#' {
					return p.errAt(at, 4, "content after --- is not supported; start it on the next line")
				}
				started = true
				continue
			case trimmed == "...":
				ended = true
				continue
			case strings.HasPrefix(trimmed, "%"):
				return p.errAt(at, 0, "directives are not supported")
			}
		}
		if trimmed != "" && trimmed[0] != '#' {
			if ended {
				return p.errAt(at, at.indent, "content after the document end (...)")
			}
			started = true
		}
		p.lines = append(p.lines, at)
	}
	p.open = len(p.lines) > 0 && p.lines[len(p.lines)-1].num == len(raws)
	return nil
}

func blank(l line) bool {
	t := strings.TrimSpace(l.text)
	return t == "" || t[0] == '#'
}

// skipBlank moves to the next line with content. That line is about to be
// read as structure, so tab indentation is an error there.
func (p *parser) skipBlank() error {
	for p.i < len(p.lines) && blank(p.lines[p.i]) {
		p.i++
	}
	if p.i < len(p.lines) && p.lines[p.i].tab {
		return p.errAt(p.lines[p.i], 0, "tabs are not allowed for indentation")
	}
	return nil
}

func isItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func isComplexKey(text string) bool { return text == "?" || strings.HasPrefix(text, "? ") }

// node parses the block node that starts on the current line, which is
// indented exactly ind.
func (p *parser) node(ind int) (any, error) {
	l := p.lines[p.i]
	if p.depth++; p.depth > maxDepth {
		return nil, p.errAt(l, l.indent, errDeep)
	}
	defer func() { p.depth-- }()
	switch {
	case isItem(l.text):
		return p.seq(ind)
	case isComplexKey(l.text):
		return nil, p.errAt(l, l.indent, "complex keys are not supported")
	}
	if _, _, _, ok := splitKey(l.text); ok {
		return p.mapping(ind)
	}
	return p.value(l.text, l.indent, ind-1, false)
}

func (p *parser) seq(ind int) (any, error) {
	out := []any{}
	for {
		if err := p.skipBlank(); err != nil {
			return nil, err
		}
		if p.i == len(p.lines) {
			return out, nil
		}
		l := &p.lines[p.i]
		if l.indent < ind || (l.indent == ind && !isItem(l.text)) {
			return out, nil
		}
		if l.indent > ind {
			return nil, p.errAt(*l, l.indent, "bad indentation")
		}
		rest := l.text[1:]
		sp := len(rest) - len(strings.TrimLeft(rest, " "))
		rest = rest[sp:]
		col := l.indent + 1 + sp
		if rest != "" && rest[0] != '#' {
			if _, _, _, ok := splitKey(rest); ok || isItem(rest) {
				// "- key: v" or "- - x": a block node that starts mid-line.
				// Treat the rest of the line as a line of its own.
				l.indent, l.text = col, rest
				v, err := p.node(col)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
				continue
			}
		}
		v, err := p.value(rest, col, ind, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *parser) mapping(ind int) (any, error) {
	out := map[string]any{}
	var merge any
	merged := false
	for {
		if err := p.skipBlank(); err != nil {
			return nil, err
		}
		if p.i == len(p.lines) {
			break
		}
		l := p.lines[p.i]
		if l.indent < ind {
			break
		}
		if l.indent > ind {
			return nil, p.errAt(l, l.indent, "bad indentation")
		}
		if isComplexKey(l.text) {
			return nil, p.errAt(l, l.indent, "complex keys are not supported")
		}
		key, rest, restAt, ok := splitKey(l.text)
		if !ok {
			return nil, p.errAt(l, l.indent, "expected a key: value")
		}
		_, dup := out[key]
		if dup || (key == "<<" && merged) {
			return nil, p.errAt(l, l.indent, fmt.Sprintf("duplicate key %q", key))
		}
		v, err := p.value(rest, l.indent+restAt, ind, true)
		if err != nil {
			return nil, err
		}
		if key == "<<" {
			merge, merged = v, true
			if err := applyMerge(out, merge, false); err != nil {
				return nil, p.errAt(l, l.indent, err.Error())
			}
			continue
		}
		out[key] = v
	}
	if merged {
		// Explicit keys win over merged ones, wherever they appear.
		_ = applyMerge(out, merge, true)
	}
	return out, nil
}

// applyMerge copies keys from a << value into out when absent. With apply
// false it only checks the value's shape. In a list, earlier maps win.
func applyMerge(out map[string]any, merge any, apply bool) error {
	var maps []map[string]any
	switch v := merge.(type) {
	case map[string]any:
		maps = append(maps, v)
	case []any:
		for _, e := range v {
			mm, ok := e.(map[string]any)
			if !ok {
				return errors.New("<< needs a mapping or a list of mappings")
			}
			maps = append(maps, mm)
		}
	default:
		return errors.New("<< needs a mapping or a list of mappings")
	}
	if apply {
		for _, mm := range maps {
			for k, v := range mm {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
	}
	return nil
}

// splitKey splits "key: rest" and reports where rest starts in text. ok is
// false when text is not a mapping entry.
func splitKey(text string) (key, rest string, restAt int, ok bool) {
	if text == "" {
		return "", "", 0, false
	}
	switch text[0] {
	case '"', '\'':
		k, n, err := quoted(text)
		if err != nil || n >= len(text) || text[n] != ':' || (n+1 < len(text) && text[n+1] != ' ') {
			return "", "", 0, false
		}
		return k, text[n+1:], n + 1, true
	case '[', '{', '#', '&', '*', '!', '|', '>', '%', '@', '`':
		return "", "", 0, false
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' && i+1 < len(text) && text[i+1] == '#' {
			return "", "", 0, false
		}
		if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ') {
			return strings.TrimRight(text[:i], " "), text[i+1:], i + 1, true
		}
	}
	return "", "", 0, false
}

// value parses the value that starts at s, which begins at column col of
// the current line, plus any lines that belong to it. parent is the indent
// of the mapping or sequence holding it; mapVal allows a sequence at the
// parent's own indent ("key:\n- a"), which YAML permits for map values.
func (p *parser) value(s string, col, parent int, mapVal bool) (any, error) {
	l := p.lines[p.i]
	t := strings.TrimLeft(s, " ")
	col += len(s) - len(t)
	switch {
	case t == "" || t[0] == '#':
		p.i++
		return p.nested(parent, mapVal)
	case t[0] == '&':
		name, rest := cutWord(t[1:])
		if name == "" {
			return nil, p.errAt(l, col, "anchor needs a name")
		}
		if strings.HasPrefix(strings.TrimLeft(rest, " "), "&") { // and each one would recurse
			return nil, p.errAt(l, col, "only one anchor per node")
		}
		v, err := p.value(rest, col+1+len(name), parent, mapVal)
		if err != nil {
			return nil, err
		}
		p.anchors[name] = v
		return v, nil
	case t[0] == '*':
		name, rest := cutWord(t[1:])
		if r := strings.TrimSpace(rest); r != "" && r[0] != '#' {
			return nil, p.errAt(l, col, "unexpected text after alias")
		}
		v, ok := p.anchors[name]
		if !ok {
			return nil, p.errAt(l, col, "undefined alias *"+name)
		}
		p.i++
		return v, nil
	case t[0] == '!':
		return nil, p.errAt(l, col, "tags are not supported")
	case isComplexKey(t):
		return nil, p.errAt(l, col, "complex keys are not supported")
	case t[0] == '|' || t[0] == '>':
		p.i++
		return p.blockScalar(t, l, col, parent)
	case t[0] == '[' || t[0] == '{':
		return p.flowValue(t, col)
	case t[0] == '"' || t[0] == '\'':
		// A quoted scalar may continue on the following lines.
		start, s := p.i, t
		str, n, err := quoted(s)
		for errors.Is(err, errUnterminated) && p.i+1 < len(p.lines) {
			// Only a line with an unescaped closing quote can end the string
			// (an escape never spans a line break): jump to it and parse once,
			// so a long or unterminated scalar costs one pass, not one per line.
			j := p.i + 1
			for j < len(p.lines) && !endsQuote(p.lines[j].raw, t[0]) {
				j++
			}
			if j == len(p.lines) {
				break
			}
			var b strings.Builder
			b.WriteString(s)
			for p.i < j {
				p.i++
				b.WriteByte('\n')
				b.WriteString(p.lines[p.i].raw)
			}
			s = b.String()
			str, n, err = quoted(s)
		}
		if err != nil {
			p.i = start
			return nil, p.errAt(l, col, err.Error())
		}
		if r := strings.TrimSpace(s[n:]); r != "" && r[0] != '#' {
			last, c := p.lines[p.i], col+n
			if p.i != start {
				c = len(last.raw) - len(s[n:])
			}
			return nil, p.errAt(last, c+len(s[n:])-len(strings.TrimLeft(s[n:], " ")), "unexpected text after quoted string")
		}
		p.i++
		return str, nil
	}
	return p.plain(t, parent), nil
}

// plain reads a plain scalar that starts at t and may continue on following
// lines indented deeper than parent. Line breaks fold into spaces; blank
// lines become line breaks. A comment, a less indented line or a line that
// looks like "key: value" ends it.
func (p *parser) plain(t string, parent int) any {
	text := stripComment(t)
	ended := text != strings.TrimRight(t, " \t")
	p.i++
	var b strings.Builder
	b.WriteString(text)
	for !ended {
		j, breaks := p.i, 0
		for j < len(p.lines) && strings.TrimSpace(p.lines[j].text) == "" {
			j++
			breaks++
		}
		if j == len(p.lines) {
			break
		}
		next := p.lines[j]
		if next.tab || next.indent <= parent || next.text[0] == '#' {
			break
		}
		if _, _, _, ok := splitKey(next.text); ok {
			break
		}
		more := stripComment(next.text)
		ended = more != strings.TrimRight(next.text, " \t")
		if breaks == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteString(strings.Repeat("\n", breaks))
		}
		b.WriteString(more)
		p.i = j + 1
	}
	if b.Len() == len(text) {
		return resolve(text)
	}
	return b.String()
}

// nested parses the block node on the following lines, if they are indented
// under parent; otherwise the value is null.
func (p *parser) nested(parent int, mapVal bool) (any, error) {
	if err := p.skipBlank(); err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		next := p.lines[p.i]
		if next.indent > parent || (mapVal && next.indent == parent && isItem(next.text)) {
			return p.node(next.indent)
		}
	}
	return nil, nil
}

func cutWord(s string) (word, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

func stripComment(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			s = s[:i]
			break
		}
	}
	return strings.TrimRight(s, " \t")
}

// blockScalar reads a | or > scalar whose header is h.
func (p *parser) blockScalar(h string, l line, col, parent int) (any, error) {
	chomp, explicit := 0, 0
	for _, c := range stripComment(h)[1:] {
		switch {
		case c == '-' && chomp == 0:
			chomp = -1
		case c == '+' && chomp == 0:
			chomp = 1
		case c >= '1' && c <= '9' && explicit == 0:
			explicit = int(c - '0')
		default:
			return nil, p.errAt(l, col, "bad block scalar header")
		}
	}
	ind := -1
	if explicit > 0 {
		ind = max(parent, 0) + explicit
	}
	var body []string
	for p.i < len(p.lines) {
		raw := p.lines[p.i].raw
		if strings.TrimSpace(raw) == "" {
			body = append(body, "")
			p.i++
			continue
		}
		in := len(raw) - len(strings.TrimLeft(raw, " "))
		if ind < 0 {
			if in <= parent {
				break
			}
			ind = in
		}
		if in < ind {
			break
		}
		body = append(body, raw[ind:])
		p.i++
	}
	trailing := 0
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		trailing++
	}
	// The file's last line has no line break: a blank one adds nothing, a
	// text one ends the scalar without a final break.
	brk := "\n"
	if p.i == len(p.lines) && p.open {
		if trailing > 0 {
			trailing--
		} else {
			brk = ""
		}
	}
	var core string
	if h[0] == '|' {
		core = strings.Join(body, "\n")
	} else {
		core = fold(body)
	}
	switch {
	case core == "" && chomp > 0:
		return strings.Repeat("\n", trailing), nil // only empty lines: each is a kept break
	case core == "" || chomp < 0:
		return core, nil
	case chomp > 0:
		return core + brk + strings.Repeat("\n", trailing), nil
	}
	return core + brk, nil
}

// fold joins the lines of a > scalar: a line break between two text lines
// becomes a space, empty lines become line breaks, and more-indented lines
// keep their breaks.
func fold(lines []string) string {
	var b strings.Builder
	more := func(s string) bool { return s != "" && (s[0] == ' ' || s[0] == '\t') }
	for i, ln := range lines {
		switch {
		case i == 0:
		case ln == "":
			b.WriteByte('\n')
			continue
		case lines[i-1] == "":
		case more(ln) || more(lines[i-1]):
			b.WriteByte('\n')
		default:
			b.WriteByte(' ')
		}
		b.WriteString(ln)
	}
	return b.String()
}

// flowValue reads a [..] or {..} collection that starts at t and may span
// lines, then parses it. Errors point at the offending token.
func (p *parser) flowValue(t string, col int) (any, error) {
	start := p.lines[p.i]
	var b strings.Builder
	depth, qAt := 0, 0
	var q byte
	s := t
	// segs maps offsets in b back to lines: each line's text starts at off.
	type seg struct {
		off, col int
		l        line
	}
	segs := []seg{{0, col, start}}
	errAt := func(i int, msg string) error {
		k := len(segs) - 1
		for segs[k].off > i {
			k--
		}
		return p.errAt(segs[k].l, segs[k].col+i-segs[k].off, msg)
	}
	for {
	scan:
		for j := 0; j < len(s); j++ {
			c := s[j]
			switch {
			case q == '"':
				if c == '\\' && j+1 < len(s) {
					b.WriteByte(c)
					j++
					c = s[j]
				} else if c == '"' {
					q = 0
				}
			case q == '\'':
				if c == '\'' {
					q = 0
				}
			case c == '"' || c == '\'':
				q, qAt = c, b.Len()
			case c == '#' && (j == 0 || s[j-1] == ' ' || s[j-1] == '\t'):
				break scan
			case c == '[' || c == '{':
				depth++
			case c == ']' || c == '}':
				depth--
			}
			b.WriteByte(c)
		}
		p.i++
		if depth <= 0 && q == 0 {
			break
		}
		if p.i == len(p.lines) {
			if q != 0 {
				return nil, errAt(qAt, errUnterminated.Error())
			}
			return nil, p.errAt(start, col, "unterminated flow collection")
		}
		b.WriteByte('\n')
		s = p.lines[p.i].raw
		segs = append(segs, seg{b.Len(), 0, p.lines[p.i]})
	}
	f := &flow{s: b.String(), anchors: p.anchors}
	v, err := f.value()
	if err != nil {
		return nil, errAt(f.i, err.Error())
	}
	f.space()
	if f.i < len(f.s) {
		return nil, errAt(f.i, "unexpected text after flow collection")
	}
	return v, nil
}

type flow struct {
	s       string
	i       int
	anchors map[string]any
	depth   int // collections open right now
}

func (f *flow) space() {
	for f.i < len(f.s) && strings.IndexByte(" \t\n", f.s[f.i]) >= 0 {
		f.i++
	}
}

func (f *flow) peek(c byte) bool { return f.i < len(f.s) && f.s[f.i] == c }

func (f *flow) value() (any, error) {
	f.space()
	if f.i == len(f.s) {
		return nil, errors.New("unexpected end of flow collection")
	}
	if f.depth++; f.depth > maxDepth {
		return nil, errors.New(errDeep)
	}
	defer func() { f.depth-- }()
	switch f.s[f.i] {
	case '[':
		f.i++
		out := []any{}
		for {
			f.space()
			if f.peek(']') {
				f.i++
				return out, nil
			}
			v, err := f.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			f.space()
			switch {
			case f.peek(','):
				f.i++
			case f.peek(']'):
				f.i++
				return out, nil
			default:
				return nil, errors.New("expected , or ] in flow sequence")
			}
		}
	case '{':
		f.i++
		out := map[string]any{}
		var merge any
		merged := false
		for {
			f.space()
			if f.peek('}') {
				break
			}
			at := f.i
			k, err := f.key()
			if err != nil {
				return nil, err
			}
			f.space()
			if !f.peek(':') {
				return nil, errors.New("expected : after flow mapping key")
			}
			f.i++
			f.space()
			var v any
			if !f.peek(',') && !f.peek('}') {
				if v, err = f.value(); err != nil {
					return nil, err
				}
			}
			if _, dup := out[k]; dup || (k == "<<" && merged) {
				f.i = at
				return nil, fmt.Errorf("duplicate key %q", k)
			}
			if k == "<<" {
				if err := applyMerge(out, v, false); err != nil {
					f.i = at
					return nil, err
				}
				merge, merged = v, true
			} else {
				out[k] = v
			}
			f.space()
			if f.peek(',') {
				f.i++
			} else if !f.peek('}') {
				return nil, errors.New("expected , or } in flow mapping")
			}
		}
		f.i++
		if merged {
			// Explicit keys win over merged ones, as in block mappings.
			_ = applyMerge(out, merge, true)
		}
		return out, nil
	case '"', '\'':
		s, n, err := quoted(f.s[f.i:])
		f.i += n
		return s, err
	case '*':
		f.i++
		name := f.plain()
		v, ok := f.anchors[name]
		if !ok {
			return nil, errors.New("undefined alias *" + name)
		}
		return v, nil
	case '&', '!':
		return nil, errors.New("anchors and tags inside flow collections are not supported")
	}
	return resolve(f.plain()), nil
}

func (f *flow) key() (string, error) {
	if f.peek('"') || f.peek('\'') {
		s, n, err := quoted(f.s[f.i:])
		f.i += n
		return s, err
	}
	return f.plain(), nil
}

// plain reads a plain scalar inside a flow collection: it ends at , ] }
// a line break, or a colon followed by a space or an end character.
func (f *flow) plain() string {
	start := f.i
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == ']' || c == '}' || c == '\n' {
			break
		}
		if c == ':' && (f.i+1 == len(f.s) || strings.IndexByte(" \t\n,]}", f.s[f.i+1]) >= 0) {
			break
		}
		f.i++
	}
	return strings.TrimSpace(f.s[start:f.i])
}

var errUnterminated = errors.New("unterminated quoted string")

// escapes maps the single-character escapes of double-quoted scalars.
var escapes = map[byte]string{
	'n': "\n", 't': "\t", '\t': "\t", 'r': "\r", '0': "\x00", 'a': "\a", 'b': "\b", 'v': "\v", 'f': "\f",
	'e': "\x1b", '"': `"`, '\\': `\`, '/': "/", ' ': " ", 'N': "\u0085", '_': "\u00a0", 'L': "\u2028", 'P': "\u2029",
}

// quoted reads a quoted scalar at the start of s and returns its value and
// the bytes consumed. s may hold several lines: a line break folds into a
// space, an empty line into a line break, and in double quotes a backslash
// before the break joins the lines with nothing between them.
func quoted(s string) (string, int, error) {
	q := s[0]
	var b []byte
	fold := func(i int) int {
		// at s[i] == '\n': drop trailing blanks, count empty lines, skip indentation
		for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
			b = b[:len(b)-1]
		}
		breaks := 0
		for i < len(s) && (s[i] == '\n' || s[i] == ' ' || s[i] == '\t') {
			if s[i] == '\n' {
				breaks++
			}
			i++
		}
		if breaks == 1 {
			b = append(b, ' ')
		} else {
			b = append(b, strings.Repeat("\n", breaks-1)...)
		}
		return i - 1
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n':
			i = fold(i)
		case q == '\'' && c == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				b = append(b, '\'')
				i++
				continue
			}
			return string(b), i + 1, nil
		case q == '"' && c == '"':
			return string(b), i + 1, nil
		case q == '"' && c == '\\':
			i++
			if i == len(s) {
				return "", 0, errUnterminated
			}
			e := s[i]
			if r, ok := escapes[e]; ok {
				b = append(b, r...)
				continue
			}
			size := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			switch {
			case e == '\n':
				for i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
					i++
				}
			case size > 0:
				if i+size >= len(s) {
					return "", 0, fmt.Errorf(`bad \%c escape`, e)
				}
				r, err := strconv.ParseUint(s[i+1:i+1+size], 16, 32)
				if err != nil || r > unicode.MaxRune {
					return "", 0, fmt.Errorf(`bad \%c escape`, e)
				}
				b = utf8.AppendRune(b, rune(r))
				i += size
			default:
				return "", 0, fmt.Errorf(`unknown escape \%c`, e)
			}
		default:
			b = append(b, c)
		}
	}
	return "", 0, errUnterminated
}

// resolve types a plain scalar by the YAML 1.2 core schema. .inf and .nan
// stay strings: JSON has no way to write them and Compose never uses them.
func resolve(t string) any {
	switch t {
	case "", "~", "null", "Null", "NULL":
		return nil
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if n, ok := number(t); ok {
		return n
	}
	return t
}

// jsonNumber is the JSON number syntax. A YAML number written that way
// keeps its text (1.10 stays 1.10, a 20-digit id stays exact), as it does
// in a JSON project file.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?$`)

// number types t. Decimal numbers that JSON could not hold as written
// (022, +1, .5) stay strings, so their text is never rewritten; numeric
// fields still parse them.
func number(t string) (json.Number, bool) {
	for prefix, base := range map[string]int{"0x": 16, "0o": 8} {
		if strings.HasPrefix(t, prefix) {
			v, err := strconv.ParseUint(t[2:], base, 64)
			if err != nil {
				return "", false
			}
			return json.Number(strconv.FormatUint(v, 10)), true
		}
	}
	if jsonNumber.MatchString(t) {
		return json.Number(t), true
	}
	return "", false
}

// endsQuote reports whether line holds a quote q that is not escaped: \" in
// double quotes, ” in single quotes. The scan starts fresh on each line.
func endsQuote(line string, q byte) bool {
	for i := 0; i < len(line); i++ {
		switch {
		case q == '"' && line[i] == '\\':
			i++
		case line[i] == q:
			if q == '\'' && i+1 < len(line) && line[i+1] == '\'' {
				i++
				continue
			}
			return true
		}
	}
	return false
}
