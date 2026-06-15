package model

import "strings"

type ProviderResult struct {
	Backend         string
	TimedOut        bool
	Stalled         bool
	Recovered       bool
	ReturnCode      int
	ReturnCodeKnown bool
	RecoverySource  string
	Warnings        []string
	RetryableError  string
	Extra           map[string]any
	present         map[string]bool
}

func ParseProviderResult(value any) (ProviderResult, bool) {
	switch typed := value.(type) {
	case ProviderResult:
		return typed.Clone(), true
	case *ProviderResult:
		if typed == nil {
			return ProviderResult{}, false
		}
		return typed.Clone(), true
	case map[string]any:
		result := ProviderResult{
			Backend:        stringFromAny(typed["backend"]),
			TimedOut:       boolFromAny(typed["timed_out"]),
			Stalled:        boolFromAny(typed["stalled"]),
			Recovered:      boolFromAny(typed["recovered"]),
			RecoverySource: stringFromAny(typed["recovery_source"]),
			Warnings:       normalizeStringItems(typed["warnings"]),
			RetryableError: stringFromAny(typed["retryable_error"]),
			Extra:          cloneUnknownProviderResultFields(typed),
			present:        providerResultPresentFields(typed),
		}
		if typed["return_code"] != nil {
			result.ReturnCode = intFromAny(typed["return_code"], 0)
			result.ReturnCodeKnown = true
		}
		return result, true
	default:
		return ProviderResult{}, false
	}
}

func NewProviderResult(backend string, returnCode int, returnCodeKnown bool) ProviderResult {
	return ProviderResult{
		Backend:         strings.TrimSpace(backend),
		ReturnCode:      returnCode,
		ReturnCodeKnown: returnCodeKnown,
		Warnings:        []string{},
		Extra:           map[string]any{},
	}
}

func (p ProviderResult) Clone() ProviderResult {
	p.Warnings = cloneStringSlice(p.Warnings)
	p.Extra = cloneMap(p.Extra)
	p.present = cloneBoolMap(p.present)
	return p
}

func (p ProviderResult) ToMap() map[string]any {
	warnings := make([]any, 0, len(p.Warnings))
	for _, warning := range p.Warnings {
		if text := strings.TrimSpace(warning); text != "" {
			warnings = append(warnings, text)
		}
	}
	payload := cloneMap(p.Extra)
	if p.shouldWrite("backend") {
		payload["backend"] = p.Backend
	}
	if p.shouldWrite("timed_out") {
		payload["timed_out"] = p.TimedOut
	}
	if p.shouldWrite("stalled") {
		payload["stalled"] = p.Stalled
	}
	if p.shouldWrite("recovered") {
		payload["recovered"] = p.Recovered
	}
	if p.shouldWrite("recovery_source") {
		payload["recovery_source"] = p.RecoverySource
	}
	if p.shouldWrite("warnings") {
		payload["warnings"] = warnings
	}
	if p.ReturnCodeKnown && p.shouldWrite("return_code") {
		payload["return_code"] = p.ReturnCode
	} else if p.shouldWrite("return_code") {
		payload["return_code"] = nil
	}
	if strings.TrimSpace(p.RetryableError) != "" || p.wasPresent("retryable_error") {
		payload["retryable_error"] = strings.TrimSpace(p.RetryableError)
	}
	return payload
}

func (p ProviderResult) wasPresent(key string) bool {
	return p.present != nil && p.present[key]
}

func (p ProviderResult) shouldWrite(key string) bool {
	if p.present == nil {
		return true
	}
	return p.present[key]
}

func cloneUnknownProviderResultFields(value map[string]any) map[string]any {
	result := map[string]any{}
	for key, item := range value {
		if providerResultKnownKeys[key] {
			continue
		}
		result[key] = cloneValue(item)
	}
	return result
}

func providerResultPresentFields(value map[string]any) map[string]bool {
	result := map[string]bool{}
	for key := range value {
		if providerResultKnownKeys[key] {
			result[key] = true
		}
	}
	return result
}

var providerResultKnownKeys = map[string]bool{
	"backend":         true,
	"timed_out":       true,
	"stalled":         true,
	"recovered":       true,
	"return_code":     true,
	"recovery_source": true,
	"warnings":        true,
	"retryable_error": true,
}

func boolFromAny(value any) bool {
	typed, _ := value.(bool)
	return typed
}
