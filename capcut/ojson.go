package capcut

import (
	"fmt"
	"strconv"
	"strings"
)

// KV / Obj form an insertion-ordered JSON object so output is byte-identical to
// Python's json.dumps(obj, ensure_ascii=False, separators=(",", ":")).
type KV struct {
	K string
	V any
}
type Obj []KV

// CompactJSON serialises like Python compact_json().
func CompactJSON(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		writeJSONString(b, x)
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(pyFloat(x))
	case Obj:
		b.WriteByte('{')
		for i, kv := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, kv.K)
			b.WriteByte(':')
			writeJSON(b, kv.V)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	default:
		panic(fmt.Sprintf("CompactJSON: unsupported type %T", v))
	}
}

// pyFloat mimics Python float repr for ordinary magnitudes.
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

// writeJSONString escapes exactly like CPython's json encoder with ensure_ascii=False.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Query is an insertion-ordered query string (Python urlencode keeps dict order).
type Query []KV

func (q Query) Encode() string {
	var b strings.Builder
	for i, kv := range q {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(quotePlus(kv.K))
		b.WriteByte('=')
		b.WriteString(quotePlus(kv.V.(string)))
	}
	return b.String()
}

// quotePlus == urllib.parse.quote_plus (safe: letters, digits, "_.-~").
func quotePlus(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
