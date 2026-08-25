package recipes

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

const (
	DefaultIntegrationBundleMaxBytes int64 = 1_048_576
	DefaultNamedInputMaxBytes        int64 = 16 * 1024 * 1024
	DefaultNamedInputTotalMaxBytes   int64 = 64 * 1024 * 1024
)

type RuntimeLimits struct {
	IntegrationBundleMaxBytes int64 `json:"integration_bundle_max_bytes"`
	NamedInputMaxBytes        int64 `json:"named_input_max_bytes"`
	NamedInputTotalMaxBytes   int64 `json:"named_input_total_max_bytes"`
}

func DefaultRuntimeLimits() RuntimeLimits {
	return RuntimeLimits{
		IntegrationBundleMaxBytes: DefaultIntegrationBundleMaxBytes,
		NamedInputMaxBytes:        DefaultNamedInputMaxBytes,
		NamedInputTotalMaxBytes:   DefaultNamedInputTotalMaxBytes,
	}
}

func (c RuntimeConfig) EffectiveLimits() RuntimeLimits {
	return runtimeLimitsWithDefaults(c.Limits)
}

func runtimeLimitsWithDefaults(limits RuntimeLimits) RuntimeLimits {
	defaults := DefaultRuntimeLimits()
	if limits.IntegrationBundleMaxBytes == 0 {
		limits.IntegrationBundleMaxBytes = defaults.IntegrationBundleMaxBytes
	}
	if limits.NamedInputMaxBytes == 0 {
		limits.NamedInputMaxBytes = defaults.NamedInputMaxBytes
	}
	if limits.NamedInputTotalMaxBytes == 0 {
		limits.NamedInputTotalMaxBytes = defaults.NamedInputTotalMaxBytes
	}
	return limits
}

func ValidateRuntimeLimits(limits RuntimeLimits) error {
	fields := []struct {
		name  string
		value int64
	}{
		{name: "integration_bundle_max_bytes", value: limits.IntegrationBundleMaxBytes},
		{name: "named_input_max_bytes", value: limits.NamedInputMaxBytes},
		{name: "named_input_total_max_bytes", value: limits.NamedInputTotalMaxBytes},
	}
	for _, field := range fields {
		if field.value <= 0 {
			return fmt.Errorf("limits.%s must be a positive integer", field.name)
		}
	}
	if limits.NamedInputTotalMaxBytes < limits.NamedInputMaxBytes {
		return fmt.Errorf(
			"limits.named_input_total_max_bytes must be greater than or equal to limits.named_input_max_bytes",
		)
	}
	return nil
}

func ParseRuntimeLimits(value any) (RuntimeLimits, error) {
	if value == nil {
		return DefaultRuntimeLimits(), nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return RuntimeLimits{}, fmt.Errorf("limits must be a table")
	}
	known := map[string]bool{
		"integration_bundle_max_bytes": true,
		"named_input_max_bytes":        true,
		"named_input_total_max_bytes":  true,
	}
	unknown := make([]string, 0)
	for key := range object {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return RuntimeLimits{}, fmt.Errorf("limits contains unknown field %q", unknown[0])
	}

	limits := DefaultRuntimeLimits()
	fields := []struct {
		name   string
		assign func(int64)
	}{
		{name: "integration_bundle_max_bytes", assign: func(value int64) { limits.IntegrationBundleMaxBytes = value }},
		{name: "named_input_max_bytes", assign: func(value int64) { limits.NamedInputMaxBytes = value }},
		{name: "named_input_total_max_bytes", assign: func(value int64) { limits.NamedInputTotalMaxBytes = value }},
	}
	for _, field := range fields {
		raw, exists := object[field.name]
		if !exists {
			continue
		}
		parsed, ok := positiveInt64(raw)
		if !ok {
			return RuntimeLimits{}, fmt.Errorf("limits.%s must be a positive integer", field.name)
		}
		field.assign(parsed)
	}
	if err := ValidateRuntimeLimits(limits); err != nil {
		return RuntimeLimits{}, err
	}
	return limits, nil
}

func positiveInt64(value any) (int64, bool) {
	var parsed int64
	switch typed := value.(type) {
	case int:
		parsed = int64(typed)
	case int8:
		parsed = int64(typed)
	case int16:
		parsed = int64(typed)
	case int32:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		parsed = int64(typed)
	case uint8:
		parsed = int64(typed)
	case uint16:
		parsed = int64(typed)
	case uint32:
		parsed = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		parsed = int64(typed)
	case float32:
		value64 := float64(typed)
		if math.Trunc(value64) != value64 || value64 > math.MaxInt64 {
			return 0, false
		}
		parsed = int64(value64)
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 {
			return 0, false
		}
		parsed = int64(typed)
	case json.Number:
		integer, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = integer
	default:
		return 0, false
	}
	return parsed, parsed > 0
}
