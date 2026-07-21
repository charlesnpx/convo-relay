package integration

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func preflightError(code string, path string, message string, details map[string]any) error {
	diagnostic := contracts.NewDiagnostic(code, contracts.DiagnosticPhasePreflight, path, message, details)
	return contracts.NewDiagnosticError(message, diagnostic)
}

func wrapPreflightError(cause error, code string, path string, message string, details map[string]any) error {
	diagnostic := contracts.NewDiagnostic(code, contracts.DiagnosticPhasePreflight, path, message, details)
	return contracts.WrapDiagnosticError(cause, message, diagnostic)
}

func rejectUnknownFields(object map[string]any, allowed []string, path string, label string, code string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	unknown := make([]string, 0)
	for key := range object {
		if !allowedSet[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	fields := make([]any, len(unknown))
	for index, field := range unknown {
		fields[index] = field
	}
	return preflightError(
		code,
		appendPointer(path, unknown[0]),
		fmt.Sprintf("%s contains unsupported field(s): %s.", label, strings.Join(unknown, ", ")),
		map[string]any{"fields": fields},
	)
}

func appendPointer(path string, segment string) string {
	escaped := strings.ReplaceAll(segment, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	return path + "/" + escaped
}

func requireObject(object map[string]any, key string, path string, code string) (map[string]any, error) {
	value, exists := object[key]
	if !exists {
		return nil, preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be an object.", key), nil)
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be an object.", key), nil)
	}
	return cloneMap(result), nil
}

func requireArray(object map[string]any, key string, path string, code string) ([]any, error) {
	value, exists := object[key]
	if !exists {
		return nil, preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be an array.", key), nil)
	}
	result, ok := value.([]any)
	if !ok {
		return nil, preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be an array.", key), nil)
	}
	return result, nil
}

func requireString(object map[string]any, key string, path string, allowEmpty bool, code string) (string, error) {
	value, exists := object[key]
	if !exists {
		return "", preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be a string.", key), nil)
	}
	text, ok := value.(string)
	if !ok {
		return "", preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be a string.", key), nil)
	}
	if !allowEmpty && strings.TrimSpace(text) == "" {
		return "", preflightError(code, appendPointer(path, key), fmt.Sprintf("%s must be a non-empty string.", key), nil)
	}
	return text, nil
}
