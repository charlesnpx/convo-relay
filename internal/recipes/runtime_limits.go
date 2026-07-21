package recipes

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

const DefaultIntegrationBundleMaxBytes int64 = 1_048_576

type RuntimeLimits struct {
	IntegrationBundleMaxBytes int64 `json:"integration_bundle_max_bytes"`
}

func DefaultRuntimeLimits() RuntimeLimits {
	return RuntimeLimits{IntegrationBundleMaxBytes: DefaultIntegrationBundleMaxBytes}
}

func (c RuntimeConfig) EffectiveLimits() RuntimeLimits {
	limits := c.Limits
	if limits.IntegrationBundleMaxBytes == 0 {
		limits.IntegrationBundleMaxBytes = DefaultIntegrationBundleMaxBytes
	}
	return limits
}

func ValidateRuntimeLimits(limits RuntimeLimits) error {
	if limits.IntegrationBundleMaxBytes <= 0 {
		return fmt.Errorf("limits.integration_bundle_max_bytes must be a positive integer")
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
	raw, exists := object["integration_bundle_max_bytes"]
	if !exists {
		return DefaultRuntimeLimits(), nil
	}
	parsed, ok := positiveInt64(raw)
	if !ok {
		return RuntimeLimits{}, fmt.Errorf("limits.integration_bundle_max_bytes must be a positive integer")
	}
	return RuntimeLimits{IntegrationBundleMaxBytes: parsed}, nil
}

func RuntimeLimitsMap(limits RuntimeLimits) map[string]any {
	if limits.IntegrationBundleMaxBytes == 0 {
		limits = DefaultRuntimeLimits()
	}
	return map[string]any{
		"integration_bundle_max_bytes": limits.IntegrationBundleMaxBytes,
	}
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
