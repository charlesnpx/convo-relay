// Package format names the small set of durable public relay formats.
package format

import (
	"fmt"
	"sort"

	"github.com/charlesnpx/convo-relay/internal/eventlog"
)

const (
	PlanV1   = "relay.plan/v1"
	EventV1  = eventlog.FormatV1
	BundleV1 = "relay.bundle/v1"

	SessionSchemaVersion = 1

	DigestClassRawBytes     = "raw-bytes"
	DigestClassSemanticJSON = "semantic-json"
)

// PublicFormats returns a copy so callers cannot change the advertised set.
func PublicFormats() map[string]string {
	return map[string]string{
		"plan":   PlanV1,
		"event":  EventV1,
		"bundle": BundleV1,
	}
}

func DigestClasses() []string {
	return []string{DigestClassRawBytes, DigestClassSemanticJSON}
}

// ValidationError marks an invalid user-supplied value without imposing an
// additional serialized protocol envelope.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }

func NewValidationError(format string, args ...any) ValidationError {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

// Materialize makes maps and slices safe to retain independently of their
// source decoder. It intentionally does not omit any values during copying.
func Materialize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = Materialize(item)
		}
		return result
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[fmt.Sprint(key)] = Materialize(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = Materialize(item)
		}
		return result
	case []string:
		result := make([]string, len(typed))
		copy(result, typed)
		return result
	default:
		return value
	}
}

func cloneObject(value map[string]any) map[string]any {
	cloned, _ := Materialize(value).(map[string]any)
	return cloned
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
