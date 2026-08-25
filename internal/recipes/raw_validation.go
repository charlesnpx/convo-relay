package recipes

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/format"
)

const (
	DiagnosticCodeInvalidRecipeRecord     = "invalid_recipe_record"
	DiagnosticCodeUnknownRecipeField      = "unknown_recipe_field"
	DiagnosticCodeUnknownLifecycleField   = "unknown_lifecycle_field"
	DiagnosticCodeInvalidRecipeField      = "invalid_recipe_field"
	DiagnosticCodeInvalidRecipeEnum       = "invalid_recipe_enum"
	DiagnosticCodeInvalidParticipantTurns = "invalid_participant_turns"
	DiagnosticCodeInvalidRecipeReducer    = "invalid_recipe_reducer"
)

var allowedRecipeFields = map[string]bool{
	"id":                   true,
	"purpose":              true,
	"participants":         true,
	"facilitator":          true,
	"reducer":              true,
	"mode":                 true,
	"max_rounds":           true,
	"participant_turns":    true,
	"result_source":        true,
	"provider_retry":       true,
	"integration_contract": true,
	"max_depth":            true,
	"auto_approval":        true,
	"lifecycle":            true,
	// Generated-source metadata is retained by normalization and is valid in
	// runtime snapshots that are reapplied with transient sources.
	"origin":              true,
	"generated_from_ref":  true,
	"generated_source":    true,
	"generated_recipe_id": true,
}

var allowedLifecycleFields = map[string]bool{
	"resume":              true,
	"steering":            true,
	"dynamic":             true,
	"workspace_isolation": true,
}

// ValidateRawRelayRecipes rejects execution-affecting configuration errors
// before NormalizeRelayRecipes has an opportunity to default or discard them.
func ValidateRawRelayRecipes(rawRecipes map[string]any) error {
	diagnostics := make([]format.Diagnostic, 0)
	for _, recipeID := range sortedObjectKeys(rawRecipes) {
		path := appendRecipePointer("/relay_recipes", recipeID)
		recipe, ok := rawRecipes[recipeID].(map[string]any)
		if !ok {
			diagnostics = append(diagnostics, recipeDiagnostic(
				DiagnosticCodeInvalidRecipeRecord,
				path,
				"Relay recipe must be a table.",
				map[string]any{"recipe_id": recipeID},
			))
			continue
		}
		diagnostics = append(diagnostics, validateRecipeRecord(recipe, path)...)
	}
	if len(diagnostics) == 0 {
		return nil
	}
	return format.NewDiagnosticError("Relay recipe configuration is invalid.", diagnostics...)
}

func validateRecipeRecord(recipe map[string]any, path string) []format.Diagnostic {
	diagnostics := make([]format.Diagnostic, 0)
	keys := sortedObjectKeys(recipe)
	for _, field := range keys {
		if allowedRecipeFields[field] {
			continue
		}
		diagnostics = append(diagnostics, recipeDiagnostic(
			DiagnosticCodeUnknownRecipeField,
			appendRecipePointer(path, field),
			fmt.Sprintf("Unknown relay recipe field %q.", field),
			map[string]any{"field": field},
		))
	}

	for _, field := range []string{"id", "purpose", "facilitator", "reducer", "integration_contract", "origin", "generated_from_ref", "generated_source", "generated_recipe_id"} {
		value, exists := recipe[field]
		if !exists {
			continue
		}
		text, ok := value.(string)
		allowEmpty := field == "purpose" || field == "facilitator" || field == "reducer"
		if !ok || (!allowEmpty && strings.TrimSpace(text) == "") {
			diagnostics = append(diagnostics, invalidRecipeField(path, field, "must be a string"))
		}
	}

	if value, exists := recipe["participants"]; exists {
		participants, ok := strictStringList(value)
		if !ok || len(participants) != 2 || strings.TrimSpace(participants[0]) == "" || strings.TrimSpace(participants[1]) == "" {
			diagnostics = append(diagnostics, recipeDiagnostic(
				"invalid_participants",
				appendRecipePointer(path, "participants"),
				"Relay recipe participants must contain exactly two non-empty strings.",
				nil,
			))
		}
	}

	validateRecipeChoice := func(field string, choices ...string) {
		value, exists := recipe[field]
		if !exists {
			return
		}
		text, ok := value.(string)
		if !ok || !stringInSet(text, choices) {
			diagnostics = append(diagnostics, recipeDiagnostic(
				DiagnosticCodeInvalidRecipeEnum,
				appendRecipePointer(path, field),
				fmt.Sprintf("%s must be one of: %s.", field, strings.Join(choices, ", ")),
				map[string]any{"field": field, "allowed": stringListAny(choices)},
			))
		}
	}
	validateRecipeChoice("mode", "cooperative", "adversarial", "steelman")
	validateRecipeChoice("auto_approval", "ask", "auto-safe", "never")
	validateRecipeChoice("result_source", "last_turn", "reducer")
	validateRecipeChoice("provider_retry", ProviderRetryAllow, ProviderRetryForbid)

	for _, field := range []string{"max_rounds", "max_depth"} {
		if value, exists := recipe[field]; exists {
			integer, ok := strictInteger(value)
			if !ok || integer < 1 || !fitsNativeInt(integer) {
				diagnostics = append(diagnostics, recipeDiagnostic(
					"invalid_"+field,
					appendRecipePointer(path, field),
					fmt.Sprintf("%s must be a positive integer.", field),
					nil,
				))
			}
		}
	}
	if value, exists := recipe["participant_turns"]; exists {
		integer, ok := strictInteger(value)
		if !ok || integer < 1 || !fitsNativeInt(integer) {
			diagnostics = append(diagnostics, recipeDiagnostic(
				DiagnosticCodeInvalidParticipantTurns,
				appendRecipePointer(path, "participant_turns"),
				"participant_turns must be a positive integer.",
				nil,
			))
		}
	}

	if rawLifecycle, exists := recipe["lifecycle"]; exists {
		lifecycle, ok := rawLifecycle.(map[string]any)
		if !ok {
			diagnostics = append(diagnostics, invalidRecipeField(path, "lifecycle", "must be a table"))
		} else {
			diagnostics = append(diagnostics, validateLifecycleRecord(lifecycle, appendRecipePointer(path, "lifecycle"))...)
		}
	}

	if stringValue(recipe["result_source"]) == "reducer" {
		if rawReducer, exists := recipe["reducer"]; exists {
			reducer, ok := rawReducer.(string)
			if !ok || strings.TrimSpace(reducer) == "" {
				diagnostics = append(diagnostics, recipeDiagnostic(
					DiagnosticCodeInvalidRecipeReducer,
					appendRecipePointer(path, "reducer"),
					"A reducer result source requires a non-empty reducer profile reference.",
					nil,
				))
			}
		}
	}

	return diagnostics
}

func validateLifecycleRecord(lifecycle map[string]any, path string) []format.Diagnostic {
	diagnostics := make([]format.Diagnostic, 0)
	for _, field := range sortedObjectKeys(lifecycle) {
		if !allowedLifecycleFields[field] {
			diagnostics = append(diagnostics, recipeDiagnostic(
				DiagnosticCodeUnknownLifecycleField,
				appendRecipePointer(path, field),
				fmt.Sprintf("Unknown recipe lifecycle field %q.", field),
				map[string]any{"field": field},
			))
			continue
		}
		choices := []string{"allow", "forbid"}
		if field == "workspace_isolation" {
			choices = []string{"inherited", "ephemeral"}
		}
		value, ok := lifecycle[field].(string)
		if !ok || !stringInSet(value, choices) {
			diagnostics = append(diagnostics, recipeDiagnostic(
				DiagnosticCodeInvalidRecipeEnum,
				appendRecipePointer(path, field),
				fmt.Sprintf("lifecycle.%s must be one of: %s.", field, strings.Join(choices, ", ")),
				map[string]any{"field": field, "allowed": stringListAny(choices)},
			))
		}
	}
	return diagnostics
}

func invalidRecipeField(path string, field string, requirement string) format.Diagnostic {
	return recipeDiagnostic(
		DiagnosticCodeInvalidRecipeField,
		appendRecipePointer(path, field),
		fmt.Sprintf("Recipe field %s %s.", field, requirement),
		map[string]any{"field": field},
	)
}

func recipeDiagnostic(code string, path string, message string, details map[string]any) format.Diagnostic {
	return format.NewDiagnostic(code, format.DiagnosticPhasePreflight, path, message, details)
}

func appendRecipePointer(path string, token string) string {
	escaped := strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	if path == "" {
		return "/" + escaped
	}
	return strings.TrimSuffix(path, "/") + "/" + escaped
}

func strictStringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		result := make([]string, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			result[index] = text
		}
		return result, true
	default:
		return nil, false
	}
}

func strictInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		value64 := float64(typed)
		if math.Trunc(value64) != value64 || !floatFitsInt64(value64) {
			return 0, false
		}
		return int64(value64), true
	case float64:
		if math.Trunc(typed) != typed || !floatFitsInt64(typed) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func floatFitsInt64(value float64) bool {
	limit := math.Exp2(63)
	return value >= -limit && value < limit
}

func fitsNativeInt(value int64) bool {
	if strconv.IntSize == 32 {
		return value >= math.MinInt32 && value <= math.MaxInt32
	}
	return true
}

func stringInSet(value string, choices []string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func stringListAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
