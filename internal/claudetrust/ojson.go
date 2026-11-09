package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Claude's config is read and rewritten without losing anything: objects keep
// their key order and every leaf (string, number, bool, null) keeps its exact
// original bytes, so large integers, floats and string escapes are untouched.
// Writing follows JSON.stringify(value, null, 2), which is how Claude Code
// writes the file, so an unchanged document re-encodes to the same bytes.

// object is a JSON object with its key order.
type object struct {
	keys []string
	vals map[string]any // *object, []any or json.RawMessage
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) (any, bool) { v, ok := o.vals[k]; return v, ok }

// set replaces k's value in place, or appends k (as JavaScript does).
func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
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
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T", kt)
				}
				v, err := decodeValue(dec, b)
				if err != nil {
					return nil, err
				}
				o.set(k, v) // a duplicate key keeps its first position and the last value, like JSON.parse
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
		// A leaf: keep its source bytes. Between the previous token and this
		// one there can only be whitespace and a ':' or ','.
		raw := bytes.TrimLeft(b[start:dec.InputOffset()], " \t\r\n:,")
		return json.RawMessage(bytes.Clone(raw)), nil
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
		if len(t.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeIndent(buf, depth+1)
			writeString(buf, k)
			buf.WriteString(": ")
			writeValue(buf, t.vals[k], depth+1)
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
