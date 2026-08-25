package recipes

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/format"
)

func mergeNamedRecords(defaults map[string]map[string]any, overrides map[string]any) map[string]map[string]any {
	merged := cloneNestedObject(defaults)
	for rawID, raw := range overrides {
		itemID := strings.TrimSpace(rawID)
		if itemID == "" {
			continue
		}
		override, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		base := map[string]any{}
		if existing, ok := merged[itemID]; ok {
			base = cloneObject(existing)
		}
		for key, value := range override {
			base[key] = format.Materialize(value)
		}
		base["id"] = itemID
		merged[itemID] = base
	}
	return merged
}

func cleanStringList(value any, lower bool) []any {
	var raw []any
	switch typed := value.(type) {
	case []any:
		raw = typed
	case []string:
		raw = make([]any, 0, len(typed))
		for _, item := range typed {
			raw = append(raw, item)
		}
	default:
		return []any{}
	}
	result := []any{}
	for _, item := range raw {
		text := strings.TrimSpace(fmt.Sprint(item))
		if text == "" {
			continue
		}
		if lower {
			text = strings.ToLower(text)
		}
		result = append(result, text)
	}
	return result
}

func stringSlice(value any) []string {
	items := cleanStringList(value, false)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}

func positiveInt(value any, fallback int) int {
	parsed, ok := parseInt(value)
	if !ok {
		return fallback
	}
	if parsed < 1 {
		return 1
	}
	return parsed
}

func intFromAny(value any, fallback int) int {
	parsed, ok := parseInt(value)
	if !ok {
		return fallback
	}
	return parsed
}

func parseInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		if !fitsNativeInt(typed) {
			return 0, false
		}
		return int(typed), true
	case int32:
		return int(typed), true
	case float64:
		if math.Trunc(typed) != typed || !floatFitsNativeInt(typed) {
			return 0, false
		}
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		parsed, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value)))
		return parsed, err == nil
	}
}

func floatFitsNativeInt(value float64) bool {
	if strconv.IntSize == 32 {
		return value >= float64(math.MinInt32) && value <= float64(math.MaxInt32)
	}
	limit := math.Exp2(63)
	return value >= -limit && value < limit
}

func normalizeMode(value any) string {
	mode := strings.TrimSpace(stringValue(value))
	switch mode {
	case "cooperative", "adversarial", "steelman":
		return mode
	default:
		return "adversarial"
	}
}

func normalizeAutoApproval(value any) string {
	mode := strings.TrimSpace(stringValue(value))
	switch mode {
	case "ask", "auto-safe", "never":
		return mode
	default:
		return "ask"
	}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func fallbackString(value any, fallback string) string {
	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return fallback
	}
	return text
}

func asObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func materializeMap(value map[string]any) map[string]any {
	materialized, ok := format.Materialize(value).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return materialized
}

func cloneNestedObject(value map[string]map[string]any) map[string]map[string]any {
	result := make(map[string]map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneObject(item)
	}
	return result
}

func cloneObject(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = format.Materialize(item)
	}
	return result
}

func sortedObjectKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func defaultCompositionPath(value string) string {
	if strings.TrimSpace(value) == "" {
		return "root"
	}
	return value
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
