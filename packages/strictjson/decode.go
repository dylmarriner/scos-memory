// Package strictjson rejects ambiguous JSON objects before authenticated handlers.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// DecodeObject accepts exactly one JSON object, rejects duplicate keys at
// every depth, and disallows unknown destination fields. Callers must bound r.
const maxJSONDepth = 64

func DecodeObject(r io.Reader, dst any) error {
	return decodeObject(r, dst, nil)
}

// DecodeObjectWithExactKeyFields preserves case-sensitive keys inside selected
// top-level map fields, while retaining case-folded duplicate rejection for
// typed envelope members. Exact duplicate keys remain forbidden at every depth.
// Use only for known untyped map fields, not for typed nested structures.
func DecodeObjectWithExactKeyFields(r io.Reader, dst any, fields ...string) error {
	exactFields := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		exactFields[canonicalMemberName(field)] = struct{}{}
	}
	return decodeObject(r, dst, exactFields)
}

func decodeObject(r io.Reader, dst any, exactFields map[string]struct{}) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	scan := json.NewDecoder(bytes.NewReader(raw))
	opening, err := scan.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('{') {
		return errors.New("JSON object required")
	}
	if err := scanObject(scan, 1, false, exactFields); err != nil {
		return err
	}
	if _, err := scan.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("exactly one JSON object required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func scanObject(dec *json.Decoder, depth int, exactKeys bool, exactFields map[string]struct{}) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting too deep")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		member, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := member.(string)
		if !ok {
			return errors.New("JSON object key must be a string")
		}
		folded := canonicalMemberName(name)
		key := folded
		if exactKeys {
			key = name
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate JSON member %q", name)
		}
		seen[key] = struct{}{}
		childExact := exactKeys
		if depth == 1 {
			_, childExact = exactFields[folded]
		}
		if err := scanValue(dec, depth+1, childExact, exactFields); err != nil {
			return err
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim('}') {
		return errors.New("invalid JSON object closing delimiter")
	}
	return nil
}

func scanValue(dec *json.Decoder, depth int, exactKeys bool, exactFields map[string]struct{}) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting too deep")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return scanObject(dec, depth, exactKeys, exactFields)
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1, exactKeys, exactFields); err != nil {
				return err
			}
		}
		closing, err := dec.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array closing delimiter")
		}
		return nil
	default:
		return errors.New("unexpected JSON closing delimiter")
	}
}

// encoding/json matches struct field names with Unicode simple case folding.
// Reject aliases that a typed decoder would map to the same destination field,
// without quadratic comparisons on attacker-controlled object key counts.
func canonicalMemberName(name string) string {
	var result strings.Builder
	result.Grow(len(name))
	for _, r := range name {
		lowest := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < lowest {
				lowest = folded
			}
		}
		result.WriteRune(lowest)
	}
	return result.String()
}
