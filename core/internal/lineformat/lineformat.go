// Package lineformat compiles proxy-list line-format templates into matchers.
//
// A format is a template built from field names separated by literal
// characters, with optional parts in brackets:
//
//	host:port:user:pass
//	[protocol://][user[:pass]@]host:port
//	host port
//	host:port:*:user:pass
//
// Fields: host (ip), port, user (username, login), pass (password, pwd),
// protocol (scheme, proto). "*" matches and discards one field. host and port
// are required and may not be inside brackets; every other field is optional
// at the user's discretion via [...].
//
// Two lenient behaviors apply to every template so mixed lists don't break: a
// leading scheme:// is stripped (and used as the protocol) when the template
// has no protocol field, and a line that is a bare host:port always parses.
package lineformat

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// PresetURL is the standard URL line format — scheme and auth optional. It is
// the default for new sources and imports.
const PresetURL = "[protocol://][user[:pass]@]host:port"

// Presets are the built-in formats offered by the dashboard. They are not
// recorded into format history.
var Presets = []string{
	PresetURL,
	"host:port:user:pass",
	"user:pass:host:port",
	"host:port@user:pass",
}

// IsPreset reports whether format is one of the built-in formats.
func IsPreset(format string) bool {
	format = strings.TrimSpace(format)
	for _, p := range Presets {
		if format == p {
			return true
		}
	}
	return false
}

// Parsed holds the fields extracted from one proxy-list line.
type Parsed struct {
	Address  string  // host:port
	Protocol string  // http|https|socks4|socks4a|socks5 — empty means "use source default"
	Username *string // nil if not present
	Password *string // nil if not present
}

var knownProtocols = map[string]bool{
	"http": true, "https": true, "socks4": true, "socks4a": true, "socks5": true,
}

// Format is a compiled line format ready to parse lines.
type Format struct {
	re          *regexp.Regexp
	hasProtocol bool
}

// Validate reports whether format is a well-formed template. The returned
// error is user-facing.
func Validate(format string) error {
	_, err := Compile(format)
	return err
}

// Compile parses a format template into a matcher. An empty template is
// rejected (it lacks the required host and port fields).
func Compile(format string) (*Format, error) {
	format = strings.TrimSpace(format)
	if len(format) > 200 {
		return nil, fmt.Errorf("format is too long (max 200 characters)")
	}

	toks, err := tokenize(format)
	if err != nil {
		return nil, err
	}
	if err := validateTokens(toks); err != nil {
		return nil, err
	}

	var sb strings.Builder
	sb.WriteByte('^')
	hasProtocol := false
	for i, t := range toks {
		switch t.kind {
		case tokOpen:
			sb.WriteString("(?:")
		case tokClose:
			sb.WriteString(")?")
		case tokLiteral:
			sb.WriteString(regexp.QuoteMeta(t.text))
		case tokField, tokSkip:
			if t.kind == tokField {
				if t.text == "protocol" {
					hasProtocol = true
				}
				fmt.Fprintf(&sb, "(?P<%s>%s)", t.text, fieldPattern(toks, i, t.text))
			} else {
				sb.WriteString("(?:" + fieldPattern(toks, i, "") + ")")
			}
		}
	}
	sb.WriteByte('$')

	re, err := regexp.Compile(sb.String())
	if err != nil {
		// Should be unreachable: every literal is quoted and groups are balanced.
		return nil, fmt.Errorf("format does not compile: %v", err)
	}
	return &Format{re: re, hasProtocol: hasProtocol}, nil
}

// Parse extracts proxy fields from one line. ok is false for blank lines,
// comments and lines that don't match the format.
func (f *Format) Parse(line string) (Parsed, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Parsed{}, false
	}

	try := line
	proto := ""
	// Read the protocol from an optional scheme:// prefix when the template
	// itself has no protocol field.
	if !f.hasProtocol {
		try, proto = stripScheme(line)
	}

	if p, ok := f.applyMatch(try, proto); ok {
		return p, true
	}
	// Bare host:port always parses, so mixed lists don't break on lines
	// without credentials.
	return parseBare(try, proto)
}

// applyMatch runs the compiled regex and validates the captured fields.
func (f *Format) applyMatch(line, defaultProto string) (Parsed, bool) {
	m := f.re.FindStringSubmatch(line)
	if m == nil {
		return Parsed{}, false
	}
	get := func(name string) string {
		if idx := f.re.SubexpIndex(name); idx >= 0 {
			return m[idx]
		}
		return ""
	}

	host, port := get("host"), get("port")
	if host == "" || !validPort(port) {
		return Parsed{}, false
	}

	p := Parsed{Address: host + ":" + port, Protocol: defaultProto}
	if scheme := strings.ToLower(get("protocol")); scheme != "" {
		if !knownProtocols[scheme] {
			return Parsed{}, false
		}
		p.Protocol = scheme
	}
	p.Username = optField(get("user"))
	p.Password = optField(get("pass"))
	return p, true
}

// parseBare accepts host:port with a valid numeric port and nothing else.
func parseBare(line, proto string) (Parsed, bool) {
	line, lineProto := stripScheme(line)
	if lineProto != "" {
		proto = lineProto
	}
	host, port, ok := strings.Cut(line, ":")
	if !ok || host == "" || strings.ContainsAny(host, "@ \t") ||
		strings.Contains(port, ":") || !validPort(port) {
		return Parsed{}, false
	}
	return Parsed{Address: line, Protocol: proto}, true
}

// stripScheme removes a leading scheme:// and returns it when it is a known
// proxy protocol. Unknown schemes are stripped but yield no protocol, so the
// line falls back to the source's default.
func stripScheme(line string) (rest, proto string) {
	idx := strings.Index(line, "://")
	if idx == -1 {
		return line, ""
	}
	scheme := strings.ToLower(line[:idx])
	if knownProtocols[scheme] {
		proto = scheme
	}
	return line[idx+3:], proto
}

// optField turns a captured field into an optional value, %-decoding
// credentials the way url.Parse does when the encoding is valid.
func optField(s string) *string {
	if s == "" {
		return nil
	}
	if dec, err := url.PathUnescape(s); err == nil {
		s = dec
	}
	return &s
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// ── Template tokenizer + validation ─────────────────────────────────────────

type tokKind int

const (
	tokField   tokKind = iota // canonical field name in text
	tokSkip                   // "*" — match and discard one field
	tokLiteral                // literal separator text
	tokOpen                   // "["
	tokClose                  // "]"
)

type token struct {
	kind tokKind
	text string
}

var fieldAliases = map[string]string{
	"host": "host", "ip": "host",
	"port": "port",
	"user": "user", "username": "user", "login": "user",
	"pass": "pass", "password": "pass", "pwd": "pass",
	"protocol": "protocol", "scheme": "protocol", "proto": "protocol",
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '[':
			toks = append(toks, token{tokOpen, "["})
			i++
		case c == ']':
			toks = append(toks, token{tokClose, "]"})
			i++
		case c == '*':
			toks = append(toks, token{tokSkip, "*"})
			i++
		case isAlpha(c):
			j := i
			for j < len(s) && isAlpha(s[j]) {
				j++
			}
			word := s[i:j]
			canon, ok := fieldAliases[strings.ToLower(word)]
			if !ok {
				return nil, fmt.Errorf("unknown field %q — valid fields are host, port, user, pass, protocol (use * to skip a field)", word)
			}
			toks = append(toks, token{tokField, canon})
			i = j
		default:
			j := i
			for j < len(s) && !isAlpha(s[j]) && s[j] != '[' && s[j] != ']' && s[j] != '*' {
				j++
			}
			toks = append(toks, token{tokLiteral, s[i:j]})
			i = j
		}
	}
	return toks, nil
}

func validateTokens(toks []token) error {
	depth := 0
	prevWasField := false
	seen := map[string]bool{}
	for _, t := range toks {
		switch t.kind {
		case tokOpen:
			depth++
		case tokClose:
			depth--
			if depth < 0 {
				return fmt.Errorf("unmatched ']'")
			}
		case tokLiteral:
			prevWasField = false
		case tokField, tokSkip:
			if prevWasField {
				return fmt.Errorf("two fields need a separator between them (e.g. host:port, not hostport)")
			}
			prevWasField = true
			if t.kind == tokField {
				if seen[t.text] {
					return fmt.Errorf("field %q appears twice", t.text)
				}
				seen[t.text] = true
				if (t.text == "host" || t.text == "port") && depth > 0 {
					return fmt.Errorf("%s is required and cannot be inside optional [...] brackets", t.text)
				}
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("unmatched '['")
	}
	if !seen["host"] || !seen["port"] {
		return fmt.Errorf("format must contain both host and port")
	}
	return nil
}

// fieldPattern picks the regex for a field: the port is digits, the protocol
// is scheme-shaped, and every other field matches up to the next literal
// separator. When that separator is "@" the field is greedy instead, so a
// password containing "@" splits at the last one like url.Parse does.
func fieldPattern(toks []token, i int, field string) string {
	switch field {
	case "port":
		return `\d{1,5}`
	case "protocol":
		return `[a-zA-Z][a-zA-Z0-9+.-]*`
	}
	next, ok := nextLiteralChar(toks, i)
	if !ok || next == '@' {
		return `.+`
	}
	return `[^` + regexp.QuoteMeta(string(next)) + `]+`
}

// nextLiteralChar finds the first character of the next literal after token i,
// looking through bracket markers (but not past another field).
func nextLiteralChar(toks []token, i int) (byte, bool) {
	for j := i + 1; j < len(toks); j++ {
		switch toks[j].kind {
		case tokLiteral:
			return toks[j].text[0], true
		case tokField, tokSkip:
			return 0, false
		}
	}
	return 0, false
}
