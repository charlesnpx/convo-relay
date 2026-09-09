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

	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
)

// CanonicalJSONBytes is the semantic-json representation used by plans and
// bundle manifests. It has no machine-local field exclusion mode.
func CanonicalJSONBytes(value any) ([]byte, error) {
	return eventlog.SemanticJSONBytes(value)
}

func SemanticJSONDigest(value any) (string, error) { return eventlog.SemanticJSONDigest(value) }

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
	if _, err := eventlog.SemanticJSONBytesRaw(data); err != nil {
		return nil, strictJSONError("invalid JSON: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
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

func strictJSONError(message string, args ...any) error {
	diagnostic := NewDiagnostic("invalid_json", DiagnosticPhaseDecode, "", fmt.Sprintf(message, args...), nil)
	return NewDiagnosticError(diagnostic.Message, diagnostic)
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
