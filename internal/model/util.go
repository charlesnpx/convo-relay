package model

import (
	"fmt"
	"strings"
)

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func normalizeStringItems(value any) []string {
	switch typed := value.(type) {
	case []string:
		items := make([]string, 0, len(typed))
		for _, raw := range typed {
			if text := strings.TrimSpace(raw); text != "" {
				items = append(items, text)
			}
		}
		return items
	case []any:
		items := make([]string, 0, len(typed))
		for _, raw := range typed {
			if text := strings.TrimSpace(fmt.Sprint(raw)); text != "" {
				items = append(items, text)
			}
		}
		return items
	default:
		return []string{}
	}
}

func stringsToAny(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
