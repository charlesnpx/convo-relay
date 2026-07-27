package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DiagnosticCodeInvalidUTF8      = "invalid_utf8"
	DiagnosticCodeInvalidJSON      = "invalid_json"
	DiagnosticCodeDuplicateJSONKey = "duplicate_json_key"
	DiagnosticCodeTrailingJSON     = "trailing_json_value"
	DiagnosticCodeInvalidJSONRoot  = "invalid_json_root"
)

// DecodeStrictJSONBytes decodes exactly one UTF-8 JSON value. Unlike the
// compatibility DecodeJSONBytes function, it rejects duplicate object keys and
// any non-whitespace content after the first value. Numbers remain json.Number.
func DecodeStrictJSONBytes(data []byte) (any, error) {
	if offset := invalidUTF8Offset(data); offset >= 0 {
		return nil, strictJSONError(
			nil,
			DiagnosticCodeInvalidUTF8,
			"",
			"JSON input must be valid UTF-8.",
			map[string]any{"offset": offset},
		)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, "")
	if err != nil {
		return nil, err
	}

	if offset := trailingJSONOffset(data, decoder.InputOffset()); offset >= 0 {
		return nil, strictJSONError(
			nil,
			DiagnosticCodeTrailingJSON,
			"",
			"JSON input must contain exactly one top-level value.",
			map[string]any{"offset": offset},
		)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, strictJSONError(nil, DiagnosticCodeTrailingJSON, "", "JSON input must contain exactly one top-level value.", nil)
		}
		return nil, strictJSONSyntaxError(decoder, "", err)
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
		return nil, strictJSONError(
			nil,
			DiagnosticCodeInvalidJSONRoot,
			"",
			"JSON payload must be an object.",
			map[string]any{"expected": "object"},
		)
	}
	return object, nil
}

func decodeStrictJSONValue(decoder *json.Decoder, path string) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, strictJSONSyntaxError(decoder, path, err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := map[string]any{}
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, strictJSONSyntaxError(decoder, path, err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, strictJSONError(nil, DiagnosticCodeInvalidJSON, path, "JSON object keys must be strings.", nil)
			}
			keyPath := appendJSONPointer(path, key)
			if seen[key] {
				return nil, strictJSONError(
					nil,
					DiagnosticCodeDuplicateJSONKey,
					keyPath,
					"JSON objects must not contain duplicate keys.",
					map[string]any{"key": key},
				)
			}
			seen[key] = true
			item, err := decodeStrictJSONValue(decoder, keyPath)
			if err != nil {
				return nil, err
			}
			object[key] = item
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, strictJSONSyntaxError(decoder, path, err)
		}
		if closing != json.Delim('}') {
			return nil, strictJSONError(nil, DiagnosticCodeInvalidJSON, path, "JSON object is not properly closed.", nil)
		}
		return object, nil

	case '[':
		items := []any{}
		for index := 0; decoder.More(); index++ {
			item, err := decodeStrictJSONValue(decoder, appendJSONPointer(path, strconv.Itoa(index)))
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, strictJSONSyntaxError(decoder, path, err)
		}
		if closing != json.Delim(']') {
			return nil, strictJSONError(nil, DiagnosticCodeInvalidJSON, path, "JSON array is not properly closed.", nil)
		}
		return items, nil
	default:
		return nil, strictJSONError(nil, DiagnosticCodeInvalidJSON, path, "JSON contains an unexpected closing delimiter.", nil)
	}
}

func strictJSONSyntaxError(decoder *json.Decoder, path string, cause error) error {
	details := map[string]any{"offset": decoder.InputOffset()}
	if cause != nil {
		details["error"] = cause.Error()
	}
	return strictJSONError(cause, DiagnosticCodeInvalidJSON, path, "JSON input is not syntactically valid.", details)
}

func strictJSONError(cause error, code string, path string, message string, details map[string]any) error {
	diagnostic := NewDiagnostic(code, DiagnosticPhaseDecode, path, message, details)
	return WrapDiagnosticError(cause, message, diagnostic)
}

func appendJSONPointer(path string, segment string) string {
	escaped := strings.ReplaceAll(segment, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	return fmt.Sprintf("%s/%s", path, escaped)
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
