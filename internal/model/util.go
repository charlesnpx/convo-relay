package model

import (
	"fmt"
	"strconv"
	"strings"
)

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		return cloneAnySlice(typed)
	case []string:
		return cloneStringSlice(typed)
	case []map[string]any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, cloneMap(item))
		}
		return items
	default:
		return typed
	}
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneValue(item)
	}
	return result
}

func cloneAnySlice(values []any) []any {
	if values == nil {
		return []any{}
	}
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = cloneValue(value)
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

func cloneBoolMap(values map[string]bool) map[string]bool {
	if values == nil {
		return nil
	}
	result := make(map[string]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
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

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case jsonNumber:
		if value, err := typed.Int64(); err == nil {
			return int(value)
		}
	case string:
		if value, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return value
		}
	}
	return fallback
}

type jsonNumber interface {
	Int64() (int64, error)
}

func asSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return cloneAnySlice(typed)
	case []string:
		return stringsToAny(typed)
	default:
		return []any{}
	}
}

func logicalSlotIDForSlotID(slotID string) string {
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return ""
	}
	if base, _, ok := strings.Cut(slotID, "_gen"); ok && strings.HasPrefix(base, "slot_") {
		return base
	}
	return slotID
}

func slotGenerationForSlotID(slotID string) int {
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return 1
	}
	_, rawGeneration, ok := strings.Cut(slotID, "_gen")
	if !ok {
		return 1
	}
	generation, err := strconv.Atoi(rawGeneration)
	if err != nil || generation < 1 {
		return 1
	}
	return generation
}
