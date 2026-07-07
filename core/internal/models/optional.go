package models

import (
	"bytes"
	"encoding/json"
)

// Optional wraps a JSON field that must distinguish "omitted" from
// "explicitly set" — including explicitly set to null. A plain pointer can't
// tell those apart, which turns partial updates into silent field clears.
// The zero value means the key was absent, so decoding a partial document
// leaves untouched fields with Present == false.
type Optional[T any] struct {
	Present bool // key appeared in the JSON document
	Null    bool // key was explicitly null
	Value   T    // decoded value when Present && !Null
}

// UnmarshalJSON records that the key was present; encoding/json only calls it
// for keys that appear in the document, which is what makes Present reliable.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Null = true
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}

// MarshalJSON round-trips the value; absent and null both encode as null.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Present || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}
