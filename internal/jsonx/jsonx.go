// Package jsonx is an order-preserving JSON model with Python-compatible
// serialisation. The gateway rewrites client request bodies and re-sends them;
// the legacy implementation worked on Python dicts, which keep insertion order,
// and its compact output (json.dumps(separators=(",", ":"),
// ensure_ascii=False)) is part of what the upstream sees. Go's map-based
// encoding/json loses order and escapes differently, so this package provides:
//
//	*Obj          ordered object (insertion order, dict semantics)
//	[]any         array
//	string, bool, nil
//	json.Number   numbers, kept as their original text
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Obj is an ordered JSON object with Python dict semantics: assigning an
// existing key keeps its position, deleting and re-adding moves it to the end.
type Obj struct {
	keys []string
	m    map[string]any
}

// NewObj returns an empty object.
func NewObj() *Obj { return &Obj{m: map[string]any{}} }

// Len returns the number of keys.
func (o *Obj) Len() int { return len(o.keys) }

// Keys returns a copy of the keys in order.
func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

// Has reports whether key exists.
func (o *Obj) Has(key string) bool { _, ok := o.m[key]; return ok }

// Get returns the value (nil when absent; use Has to tell apart from JSON null).
func (o *Obj) Get(key string) any { return o.m[key] }

// Lookup returns the value and whether it exists.
func (o *Obj) Lookup(key string) (any, bool) { v, ok := o.m[key]; return v, ok }

// Set assigns key, keeping the position of an existing key.
func (o *Obj) Set(key string, v any) {
	if _, ok := o.m[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.m[key] = v
}

// Delete removes key; reports whether it existed (dict.pop).
func (o *Obj) Delete(key string) bool {
	if _, ok := o.m[key]; !ok {
		return false
	}
	delete(o.m, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

// Clone makes a shallow copy (dict(o)): nested values are shared.
func (o *Obj) Clone() *Obj {
	c := &Obj{keys: append([]string(nil), o.keys...), m: make(map[string]any, len(o.m))}
	for k, v := range o.m {
		c.m[k] = v
	}
	return c
}

// Each calls fn for every key in order.
func (o *Obj) Each(fn func(key string, v any)) {
	for _, k := range o.keys {
		fn(k, o.m[k])
	}
}

// Str returns the value if it is a string.
func (o *Obj) Str(key string) (string, bool) {
	s, ok := o.m[key].(string)
	return s, ok
}

// Obj returns the value if it is an object.
func (o *Obj) Obj(key string) (*Obj, bool) {
	x, ok := o.m[key].(*Obj)
	return x, ok
}

// Arr returns the value if it is an array.
func (o *Obj) Arr(key string) ([]any, bool) {
	x, ok := o.m[key].([]any)
	return x, ok
}

// ------------------------------------------------------------------ decode

// Unmarshal parses one JSON value preserving object key order. Duplicate keys
// keep the first position and the last value, like Python's json.loads.
func Unmarshal(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("non-string object key %v", kt)
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(key, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
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
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return tok, nil // string, json.Number, bool, nil
	}
}

// UnmarshalObj parses data and requires a JSON object.
func UnmarshalObj(data []byte) (*Obj, error) {
	v, err := Unmarshal(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*Obj)
	if !ok {
		return nil, errors.New("expected a JSON object")
	}
	return o, nil
}

// ------------------------------------------------------------------ encode

// Marshal renders v compactly like Python's
// json.dumps(v, ensure_ascii=False, separators=(",", ":")).
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, ",", ":"); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// MarshalPy renders v like Python's default json.dumps(ensure_ascii=False):
// separators ", " and ": ".
func MarshalPy(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, ", ", ": "); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any, itemSep, keySep string) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, t)
	case json.Number:
		if t == "" {
			b.WriteString("0")
		} else {
			b.WriteString(string(t))
		}
	case int:
		fmt.Fprintf(b, "%d", t)
	case int64:
		fmt.Fprintf(b, "%d", t)
	case float64:
		nb, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(nb)
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if err := write(b, x, itemSep, keySep); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeString(b, x)
		}
		b.WriteByte(']')
	case *Obj:
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeString(b, k)
			b.WriteString(keySep)
			if err := write(b, t.m[k], itemSep, keySep); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case json.RawMessage:
		b.Write(t)
	default:
		return fmt.Errorf("jsonx: unsupported type %T", v)
	}
	return nil
}

const hexdigits = "0123456789abcdef"

// writeString escapes like CPython's json encoder with ensure_ascii=False:
// only '"', '\\' and control characters below 0x20 are escaped.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
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
				if c < 0x20 {
					b.WriteString(`\u00`)
					b.WriteByte(hexdigits[c>>4])
					b.WriteByte(hexdigits[c&0xf])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString("�")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// ------------------------------------------------------------ Python helpers

// Truthy mimics Python truthiness for decoded JSON values.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	}
	return true
}

// PyStr mimics str(x) for scalars (used where the legacy code stringifies).
func PyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return string(t)
	}
	b, _ := MarshalPy(v)
	return string(b)
}

// Lower returns strings.ToLower of a string value, "" otherwise.
func Lower(v any) string {
	if s, ok := v.(string); ok {
		return strings.ToLower(s)
	}
	return ""
}

// PyStrIfTruthy is Python's `str(x or "")`.
func PyStrIfTruthy(v any) string {
	if !Truthy(v) {
		return ""
	}
	return PyStr(v)
}
