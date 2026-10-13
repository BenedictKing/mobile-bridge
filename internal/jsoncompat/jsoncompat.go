// Package jsoncompat keeps JSON byte-compatible with Python's json module.
//
// The gateway serializes views and persists state with
// json.dumps(..., ensure_ascii=False, separators=(",", ":")) (compact) or the
// default separators (", ", ": ") for plain HTTP bodies. Go's encoding/json
// differs in separator, HTML escaping, \u2028 handling and map ordering, so
// every value that round-trips through disk or wire is represented here as an
// insertion-ordered tree and encoded by this package.
//
// Value is one of: nil, bool, string, json.Number, *Object, *Array.
// Numbers are never parsed to float64/int64 — the original lexeme travels
// through unchanged (Python emits 1.0, Go would emit 1; a 20-digit int
// overflows int64).
package jsoncompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Object is an insertion-ordered string-keyed map. Setting an existing key
// replaces the value in place, matching Python dict semantics.
type Object struct {
	keys   []string
	values map[string]any
}

func NewObject() *Object {
	return &Object{values: map[string]any{}}
}

func (o *Object) Set(key string, value any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *Object) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// MustGet returns the value or nil for a missing key, mirroring dict.get.
func (o *Object) MustGet(key string) any {
	return o.values[key]
}

// Delete removes a key, preserving the order of the rest.
func (o *Object) Delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, existing := range o.keys {
		if existing == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *Object) Keys() []string { return o.keys }

func (o *Object) Len() int { return len(o.keys) }

// Array is an ordered JSON array.
type Array struct{ Items []any }

// Num wraps an integer as a number value.
func Num(i int) json.Number { return json.Number(strconv.Itoa(i)) }

// Unmarshal decodes JSON into an ordered Value tree. Object key order follows
// the document; duplicate keys keep their first position and last value, as
// Python's dict does.
func Unmarshal(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err == nil {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return decodeFromToken(decoder, token)
}

func decodeFromToken(decoder *json.Decoder, token json.Token) (any, error) {
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			object := NewObject()
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T, not string", keyToken)
				}
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				object.Set(key, value)
			}
			if _, err := decoder.Token(); err != nil { // consume '}'
				return nil, err
			}
			return object, nil
		case '[':
			array := &Array{}
			for decoder.More() {
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				array.Items = append(array.Items, value)
			}
			if _, err := decoder.Token(); err != nil { // consume ']'
				return nil, err
			}
			return array, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return token, nil // string, bool, nil, json.Number
	}
}

// EncodeCompact serializes with Python's separators=(",", ":").
func EncodeCompact(value any) []byte { return encode(value, ",", ":", false) }

// EncodePythonDefault serializes with Python's default separators (", ", ": ").
func EncodePythonDefault(value any) []byte { return encode(value, ", ", ": ", false) }

// EncodePythonDefaultASCII serializes with json.dumps defaults including
// ensure_ascii=True: (", ", ": ") separators and \uXXXX escaping for all
// non-ASCII (astral runes as surrogate pairs). This is the control-file and
// pairing-response format.
func EncodePythonDefaultASCII(value any) []byte { return encode(value, ", ", ": ", true) }

// EncodePythonIndent2 mirrors json.dumps(value, ensure_ascii=False, indent=2):
// two-space indentation, ",\n" item separator, ": " key separator. Empty
// containers collapse to {} / [].
func EncodePythonIndent2(value any) []byte {
	var buffer bytes.Buffer
	writeIndented(&buffer, value, 0)
	return buffer.Bytes()
}

func writeIndented(buffer *bytes.Buffer, value any, depth int) {
	switch v := value.(type) {
	case *Object:
		if v.Len() == 0 {
			buffer.WriteString("{}")
			return
		}
		buffer.WriteString("{\n")
		for i, key := range v.keys {
			if i > 0 {
				buffer.WriteString(",\n")
			}
			buffer.WriteString(strings.Repeat("  ", depth+1))
			writePythonString(buffer, key, false)
			buffer.WriteString(": ")
			writeIndented(buffer, v.values[key], depth+1)
		}
		buffer.WriteByte('\n')
		buffer.WriteString(strings.Repeat("  ", depth))
		buffer.WriteByte('}')
	case *Array:
		if len(v.Items) == 0 {
			buffer.WriteString("[]")
			return
		}
		buffer.WriteString("[\n")
		for i, item := range v.Items {
			if i > 0 {
				buffer.WriteString(",\n")
			}
			buffer.WriteString(strings.Repeat("  ", depth+1))
			writeIndented(buffer, item, depth+1)
		}
		buffer.WriteByte('\n')
		buffer.WriteString(strings.Repeat("  ", depth))
		buffer.WriteByte(']')
	default:
		writeValue(buffer, value, ",", ": ", false)
	}
}

// EncodePythonDefaultSorted mirrors json.dumps(value, ensure_ascii=False,
// sort_keys=True): default separators with recursively sorted object keys.
func EncodePythonDefaultSorted(value any) []byte {
	var buffer bytes.Buffer
	writeSorted(&buffer, value, 0)
	return buffer.Bytes()
}

func writeSorted(buffer *bytes.Buffer, value any, depth int) {
	switch v := value.(type) {
	case *Object:
		keys := append([]string{}, v.keys...)
		sort.Strings(keys)
		buffer.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buffer.WriteString(", ")
			}
			writePythonString(buffer, key, false)
			buffer.WriteString(": ")
			writeSorted(buffer, v.values[key], depth)
		}
		buffer.WriteByte('}')
	case *Array:
		buffer.WriteByte('[')
		for i, item := range v.Items {
			if i > 0 {
				buffer.WriteString(", ")
			}
			writeSorted(buffer, item, depth)
		}
		buffer.WriteByte(']')
	default:
		writeValue(buffer, value, ", ", ": ", false)
	}
}

// Clone deep-copies a value tree, preserving object key order.
func Clone(value any) any {
	switch v := value.(type) {
	case *Object:
		copied := NewObject()
		for _, key := range v.keys {
			copied.Set(key, Clone(v.values[key]))
		}
		return copied
	case *Array:
		copied := &Array{Items: make([]any, len(v.Items))}
		for i, item := range v.Items {
			copied.Items[i] = Clone(item)
		}
		return copied
	default:
		return v
	}
}

func encode(value any, itemSep, keySep string, ascii bool) []byte {
	var buffer bytes.Buffer
	writeValue(&buffer, value, itemSep, keySep, ascii)
	return buffer.Bytes()
}

func writeValue(buffer *bytes.Buffer, value any, itemSep, keySep string, ascii bool) {
	switch v := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if v {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case json.Number:
		buffer.WriteString(v.String())
	case string:
		writePythonString(buffer, v, ascii)
	case *Object:
		buffer.WriteByte('{')
		for i, key := range v.keys {
			if i > 0 {
				buffer.WriteString(itemSep)
			}
			writePythonString(buffer, key, ascii)
			buffer.WriteString(keySep)
			writeValue(buffer, v.values[key], itemSep, keySep, ascii)
		}
		buffer.WriteByte('}')
	case *Array:
		buffer.WriteByte('[')
		for i, item := range v.Items {
			if i > 0 {
				buffer.WriteString(itemSep)
			}
			writeValue(buffer, item, itemSep, keySep, ascii)
		}
		buffer.WriteByte(']')
	case int:
		buffer.WriteString(strconv.Itoa(v))
	case int64:
		buffer.WriteString(strconv.FormatInt(v, 10))
	default:
		panic(fmt.Sprintf("jsoncompat: unsupported value %T", value))
	}
}

// writePythonString matches json.dumps(s, ensure_ascii=...): \" and \\ for
// quotes and backslashes, short escapes for \b\t\n\f\r, \u00xx (lowercase hex)
// for other control characters, raw UTF-8 for everything else in non-ASCII
// mode — no HTML escaping, no \u2028/\u2029 escaping. In ASCII mode every
// rune above 0x7F becomes \uXXXX (surrogate pairs above 0xFFFF).
func writePythonString(buffer *bytes.Buffer, s string, ascii bool) {
	const hexDigits = "0123456789abcdef"
	buffer.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\t':
			buffer.WriteString(`\t`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\r':
			buffer.WriteString(`\r`)
		default:
			if r < 0x20 {
				buffer.WriteString(`\u00`)
				buffer.WriteByte(hexDigits[r>>4])
				buffer.WriteByte(hexDigits[r&0xf])
			} else if ascii && r > 0x7f {
				if r > 0xffff {
					surrogate := r - 0x10000
					writeUnicodeEscape(buffer, 0xd800+(surrogate>>10))
					writeUnicodeEscape(buffer, 0xdc00+(surrogate&0x3ff))
				} else {
					writeUnicodeEscape(buffer, r)
				}
			} else {
				buffer.WriteRune(r)
			}
		}
	}
	buffer.WriteByte('"')
}

func writeUnicodeEscape(buffer *bytes.Buffer, r rune) {
	const hexDigits = "0123456789abcdef"
	buffer.WriteString(`\u`)
	buffer.WriteByte(hexDigits[(r>>12)&0xf])
	buffer.WriteByte(hexDigits[(r>>8)&0xf])
	buffer.WriteByte(hexDigits[(r>>4)&0xf])
	buffer.WriteByte(hexDigits[r&0xf])
}

// Truthy mirrors Python truthiness for JSON values.
func Truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		number, err := strconv.ParseFloat(v.String(), 64)
		return err != nil || number != 0
	case *Object:
		return v.Len() > 0
	case *Array:
		return len(v.Items) > 0
	default:
		return true
	}
}

// PyStr mirrors Python str() for attachment metadata coercion. Nested
// containers fall back to a repr-style rendering; real payloads only carry
// scalars here.
func PyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return v
	case json.Number:
		return v.String()
	case *Object:
		entries := make([]string, 0, v.Len())
		for _, key := range v.Keys() {
			entry, _ := v.Get(key)
			entries = append(entries, "'"+key+"': "+PyStr(entry))
		}
		return "{" + strings.Join(entries, ", ") + "}"
	case *Array:
		items := make([]string, 0, len(v.Items))
		for _, item := range v.Items {
			items = append(items, PyStr(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return fmt.Sprintf("%v", value)
	}
}
