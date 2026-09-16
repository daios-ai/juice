// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// jsonEncodeNoEscape encodes v to JSON without HTML-escaping <, >, or &.
// RFC 8785 requires ECMAScript-compatible serialization; Go's json.Marshal
// escapes those characters by default, which would produce wrong signatures.
func jsonEncodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CanonicalJSON serializes v per RFC 8785: objects with Unicode-sorted keys, recursively.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return canonicalValue(decoded)
}

func canonicalValue(v any) ([]byte, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyJSON, err := jsonEncodeNoEscape(k)
			if err != nil {
				return nil, err
			}
			buf.Write(keyJSON)
			buf.WriteByte(':')
			valJSON, err := canonicalValue(val[k])
			if err != nil {
				return nil, err
			}
			buf.Write(valJSON)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			itemJSON, err := canonicalValue(item)
			if err != nil {
				return nil, err
			}
			buf.Write(itemJSON)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	case float64:
		// RFC 8785: integer-valued floats are serialized without decimal point.
		if !math.IsInf(val, 0) && !math.IsNaN(val) && val == math.Trunc(val) && math.Abs(val) < 1e15 {
			return []byte(strconv.FormatInt(int64(val), 10)), nil
		}
		return json.Marshal(val)
	case string:
		return jsonEncodeNoEscape(val)
	default:
		return json.Marshal(v)
	}
}

// jcsHashStr parses a JSON string, canonicalizes it, and returns hex SHA-256.
// If s is empty, returns sha256 of empty bytes.
func jcsHashStr(jsonStr string) (string, error) {
	if jsonStr == "" {
		h := sha256.Sum256(nil)
		return fmt.Sprintf("%x", h), nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(jsonStr), &decoded); err != nil {
		h := sha256.Sum256([]byte(jsonStr))
		return fmt.Sprintf("%x", h), nil
	}
	canonical, err := canonicalValue(decoded)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", h), nil
}
