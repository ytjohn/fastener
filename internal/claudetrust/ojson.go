package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Claude's config is read and rewritten without losing anything: objects keep
// their members in order, every object key keeps its exact source bytes (so
// a key with a lone-surrogate escape or invalid UTF-8 is not turned into
// U+FFFD, and duplicate keys stay as they were), and every leaf (string,
// number, bool, null) keeps its exact source bytes, so large integers, floats
// and string escapes are untouched. Writing follows JSON.stringify(value,
// null, 2), which is how Claude Code writes the file, so an unchanged
// document re-encodes to the same bytes.

// member is one object member: its key as decoded, the key's source bytes
// (with quotes), and its value.
type member struct {
	key   string
	raw   []byte
	value any // *object, []any or json.RawMessage
}

// object is a JSON object with its members in source order.
type object struct{ members []member }

func newObject() *object { return &object{} }

// lossless reports whether a decoded key is known to be exact: the decoder
// turns a lone surrogate or invalid UTF-8 into U+FFFD, so a key containing
// U+FFFD is never matched by name (it can still be written back verbatim).
func lossless(k string) bool { return !strings.ContainsRune(k, utf8.RuneError) }

// get returns the value of the last member named k, the one JSON.parse
// would keep.
func (o *object) get(k string) (any, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if m := o.members[i]; m.key == k && lossless(m.key) {
			return m.value, true
		}
	}
	return nil, false
}

// set replaces the value of the last member named k, or appends a member
// (as assigning a new property does in JavaScript). k must be lossless.
func (o *object) set(k string, v any) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if m := &o.members[i]; m.key == k && lossless(m.key) {
			m.value = v
			return
		}
	}
	var b bytes.Buffer
	writeString(&b, k)
	o.members = append(o.members, member{key: k, raw: b.Bytes(), value: v})
}

// decodeOrdered parses one JSON value from b.
func decodeOrdered(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec, b)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the top-level value")
	}
	return v, nil
}

// rawToken is the source of the token that ended at the decoder's offset and
// started after start: between two tokens there can only be whitespace and
// a ':' or ','.
func rawToken(dec *json.Decoder, b []byte, start int64) []byte {
	return bytes.Clone(bytes.TrimLeft(b[start:dec.InputOffset()], " \t\r\n:,"))
}

func decodeValue(dec *json.Decoder, b []byte) (any, error) {
	start := dec.InputOffset()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				ks := dec.InputOffset()
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T", kt)
				}
				raw := rawToken(dec, b, ks)
				v, err := decodeValue(dec, b)
				if err != nil {
					return nil, err
				}
				o.members = append(o.members, member{key: k, raw: raw, value: v})
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec, b)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return json.RawMessage(rawToken(dec, b, start)), nil
	}
}

// encodeStringify writes v as JSON.stringify(v, null, 2) does.
func encodeStringify(v any) []byte {
	var buf bytes.Buffer
	writeValue(&buf, v, 0)
	return buf.Bytes()
}

func writeIndent(buf *bytes.Buffer, depth int) {
	buf.WriteByte('\n')
	for i := 0; i < depth; i++ {
		buf.WriteString("  ")
	}
}

func writeValue(buf *bytes.Buffer, v any, depth int) {
	switch t := v.(type) {
	case *object:
		if len(t.members) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, m := range t.members {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeIndent(buf, depth+1)
			buf.Write(m.raw)
			buf.WriteString(": ")
			writeValue(buf, m.value, depth+1)
		}
		writeIndent(buf, depth)
		buf.WriteByte('}')
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeIndent(buf, depth+1)
			writeValue(buf, e, depth+1)
		}
		writeIndent(buf, depth)
		buf.WriteByte(']')
	case json.RawMessage:
		buf.Write(t)
	default:
		panic(fmt.Sprintf("claudetrust: cannot encode %T", v))
	}
}

// writeString quotes s as JSON.stringify does: only '"', '\\' and control
// characters are escaped (as \b \t \n \f \r or \u00xx); everything else,
// including non-ASCII and U+2028/U+2029, is written as is.
func writeString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if c < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[c>>4])
				buf.WriteByte(hex[c&0xf])
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}
