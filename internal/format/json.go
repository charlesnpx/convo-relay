package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/eventlog"
)

// CanonicalJSONBytes is the semantic-json representation used by plans and
// bundle manifests. It has no machine-local field exclusion mode.
func CanonicalJSONBytes(value any) ([]byte, error) {
	return eventlog.SemanticJSONBytes(value)
}

func RawBytesDigest(data []byte) string { return eventlog.RawBytesDigest(data) }

func SemanticJSONDigest(value any) (string, error) { return eventlog.SemanticJSONDigest(value) }

func SemanticJSONDigestBytes(data []byte) (string, error) {
	return eventlog.SemanticJSONDigestBytes(data)
}

// ReadBytesLimited rejects a source before it can exceed its stated byte
// ceiling. It is used for configuration inputs, not durable payload storage.
func ReadBytesLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("reader is required")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("max bytes must be positive")
	}
	readLimit := maxBytes
	if maxBytes < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(reader, readLimit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func ReadFileBytesLimited(filename string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("max bytes must be positive")
	}
	info, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filename)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", filename, maxBytes)
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := ReadBytesLimited(file, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	return data, nil
}

// DecodeStrictJSONBytes accepts exactly one UTF-8 JSON value, preserves JSON
// numbers, and rejects duplicate object keys.
func DecodeStrictJSONBytes(data []byte) (any, error) {
	if offset := invalidUTF8Offset(data); offset >= 0 {
		return nil, strictJSONError("invalid UTF-8 at byte %d", offset)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if offset := trailingJSONOffset(data, decoder.InputOffset()); offset >= 0 {
		return nil, strictJSONError("trailing JSON content at byte %d", offset)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, strictJSONError("JSON input must contain exactly one top-level value")
		}
		return nil, strictJSONError("invalid JSON: %v", err)
	}
	return value, nil
}

func DecodeStrictJSONObjectBytes(data []byte) (map[string]any, error) {
	value, err := DecodeStrictJSONBytes(data)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, strictJSONError("JSON payload must be an object")
	}
	return object, nil
}

func decodeStrictJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, strictJSONError("invalid JSON: %v", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, strictJSONError("invalid JSON: %v", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, strictJSONError("JSON object keys must be strings")
			}
			if _, exists := object[key]; exists {
				return nil, strictJSONError("JSON objects must not contain duplicate keys")
			}
			item, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = item
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, strictJSONError("JSON object is not properly closed")
		}
		return object, nil
	case '[':
		items := []any{}
		for decoder.More() {
			item, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, strictJSONError("JSON array is not properly closed")
		}
		return items, nil
	default:
		return nil, strictJSONError("JSON contains an unexpected closing delimiter")
	}
}

func strictJSONError(message string, args ...any) error {
	diagnostic := NewDiagnostic("invalid_json", DiagnosticPhaseDecode, "", fmt.Sprintf(message, args...), nil)
	return NewDiagnosticError(diagnostic.Message, diagnostic)
}

func invalidUTF8Offset(data []byte) int {
	for offset := 0; offset < len(data); {
		runeValue, size := utf8.DecodeRune(data[offset:])
		if runeValue == utf8.RuneError && size == 1 {
			return offset
		}
		offset += size
	}
	return -1
}

func trailingJSONOffset(data []byte, inputOffset int64) int {
	offset := int(inputOffset)
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		offset = len(data)
	}
	for offset < len(data) {
		switch data[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return -1
}

func exactInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		if integer, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
			return integer, true
		}
		// semantic-json canonicalizes integral values such as 17 as 1.7e1.
		// Decode those exactly rather than rounding through float64.
		rational, ok := new(big.Rat).SetString(typed.String())
		if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
			return 0, false
		}
		return rational.Num().Int64(), true
	default:
		return 0, false
	}
}

func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && strings.TrimSpace(text) != ""
}
