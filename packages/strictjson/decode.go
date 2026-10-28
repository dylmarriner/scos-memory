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
	if err := scanObject(scan, 1); err != nil {
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

func scanObject(dec *json.Decoder, depth int) error {
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
		if _, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("duplicate JSON member %q", name)
		}
		seen[folded] = struct{}{}
		if err := scanValue(dec, depth+1); err != nil {
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

func scanValue(dec *json.Decoder, depth int) error {
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
		return scanObject(dec, depth)
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1); err != nil {
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
