package recipes

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigDefaultsIntegrationBundleLimit(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := config.Limits.IntegrationBundleMaxBytes; got != DefaultIntegrationBundleMaxBytes {
		t.Fatalf("integration bundle max bytes = %d, want %d", got, DefaultIntegrationBundleMaxBytes)
	}

	config, err = LoadRuntimeConfig(writeSettings(t, "[limits]\n"))
	if err != nil {
		t.Fatalf("load empty limits table: %v", err)
	}
	if got := config.Limits.IntegrationBundleMaxBytes; got != DefaultIntegrationBundleMaxBytes {
		t.Fatalf("empty-table limit = %d, want %d", got, DefaultIntegrationBundleMaxBytes)
	}
}

func TestLoadRuntimeConfigAcceptsPositiveIntegrationBundleLimits(t *testing.T) {
	for _, value := range []int64{1, 2_097_152, math.MaxInt64} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			settings := writeSettings(t, fmt.Sprintf("[limits]\nintegration_bundle_max_bytes = %d\n", value))
			config, err := LoadRuntimeConfig(settings)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if got := config.Limits.IntegrationBundleMaxBytes; got != value {
				t.Fatalf("limit = %d, want %d", got, value)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsInvalidIntegrationBundleLimits(t *testing.T) {
	tests := map[string]string{
		"zero":       "0",
		"negative":   "-1",
		"fractional": "1.5",
		"string":     `"1024"`,
		"boolean":    "true",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			settings := writeSettings(t, "[limits]\nintegration_bundle_max_bytes = "+value+"\n")
			_, err := LoadRuntimeConfig(settings)
			if err == nil || !strings.Contains(err.Error(), "limits.integration_bundle_max_bytes must be a positive integer") {
				t.Fatalf("error = %v", err)
			}
		})
	}

	settings := writeSettings(t, "limits = 1\n")
	if _, err := LoadRuntimeConfig(settings); err == nil || !strings.Contains(err.Error(), "limits must be a table") {
		t.Fatalf("non-table limits error = %v", err)
	}
}

func TestApplyTransientRecipeSourcesPreservesRuntimeLimits(t *testing.T) {
	config, err := LoadRuntimeConfig(writeSettings(t, "[limits]\nintegration_bundle_max_bytes = 2048\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	applied, _, err := ApplyTransientRecipeSources(config, nil)
	if err != nil {
		t.Fatalf("apply transient sources: %v", err)
	}
	if applied.Limits.IntegrationBundleMaxBytes != 2048 {
		t.Fatalf("applied limits = %#v", applied.Limits)
	}
	config.Limits.IntegrationBundleMaxBytes = -1
	if _, _, err := ApplyTransientRecipeSources(config, nil); err == nil {
		t.Fatal("negative programmatic limit unexpectedly accepted")
	}
}

func TestRuntimeConfigEffectiveLimitsDefaultsProgrammaticZeroValue(t *testing.T) {
	if got := (RuntimeConfig{}).EffectiveLimits(); got != DefaultRuntimeLimits() {
		t.Fatalf("effective limits = %#v", got)
	}
	custom := RuntimeConfig{Limits: RuntimeLimits{IntegrationBundleMaxBytes: 4096}}
	if got := custom.EffectiveLimits().IntegrationBundleMaxBytes; got != 4096 {
		t.Fatalf("custom effective limit = %d", got)
	}
}

func TestRuntimeConfigJSONExposesNestedLimits(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode config JSON: %v", err)
	}
	limits := payload["limits"].(map[string]any)
	if limits["integration_bundle_max_bytes"] != float64(DefaultIntegrationBundleMaxBytes) {
		t.Fatalf("runtime config limits JSON = %#v", limits)
	}
}
