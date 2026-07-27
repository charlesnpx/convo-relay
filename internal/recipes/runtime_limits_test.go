package recipes

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigAppliesExactLegacyCompatibleDefaults(t *testing.T) {
	config, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.Limits != DefaultRuntimeLimits() {
		t.Fatalf("default limits = %#v, want %#v", config.Limits, DefaultRuntimeLimits())
	}

	config, err = LoadRuntimeConfig(writeSettings(t, "[limits]\n"))
	if err != nil {
		t.Fatalf("load empty limits table: %v", err)
	}
	if config.Limits != (RuntimeLimits{
		IntegrationBundleMaxBytes:   1_048_576,
		NamedInputMaxBytes:          16_777_216,
		NamedInputTotalMaxBytes:     67_108_864,
		RepositoryInventoryMaxFiles: 100_000,
		RepositoryInventoryMaxBytes: 2_147_483_648,
	}) {
		t.Fatalf("empty-table limits = %#v", config.Limits)
	}
}

func TestLoadRuntimeConfigAcceptsCustomRuntimeLimits(t *testing.T) {
	settings := writeSettings(t, `[limits]
integration_bundle_max_bytes = 2
named_input_max_bytes = 3
named_input_total_max_bytes = 4
repository_inventory_max_files = 5
repository_inventory_max_bytes = 6
`)
	config, err := LoadRuntimeConfig(settings)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	want := RuntimeLimits{
		IntegrationBundleMaxBytes:   2,
		NamedInputMaxBytes:          3,
		NamedInputTotalMaxBytes:     4,
		RepositoryInventoryMaxFiles: 5,
		RepositoryInventoryMaxBytes: 6,
	}
	if config.Limits != want {
		t.Fatalf("custom limits = %#v, want %#v", config.Limits, want)
	}

	for _, value := range []int64{1, 2_097_152, math.MaxInt64} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			limits, err := ParseRuntimeLimits(map[string]any{
				"integration_bundle_max_bytes": value,
			})
			if err != nil || limits.IntegrationBundleMaxBytes != value {
				t.Fatalf("parsed limit = %#v, %v", limits, err)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsInvalidRuntimeLimits(t *testing.T) {
	tests := map[string]string{
		"zero":       "0",
		"negative":   "-1",
		"fractional": "1.5",
		"string":     `"1024"`,
		"boolean":    "true",
	}
	fields := []string{
		"integration_bundle_max_bytes",
		"named_input_max_bytes",
		"named_input_total_max_bytes",
		"repository_inventory_max_files",
		"repository_inventory_max_bytes",
	}
	for _, field := range fields {
		for name, value := range tests {
			t.Run(field+"/"+name, func(t *testing.T) {
				settings := writeSettings(t, "[limits]\n"+field+" = "+value+"\n")
				_, err := LoadRuntimeConfig(settings)
				if err == nil || !strings.Contains(err.Error(), "limits."+field+" must be a positive integer") {
					t.Fatalf("error = %v", err)
				}
			})
		}
		t.Run(field+"/overflow", func(t *testing.T) {
			_, err := ParseRuntimeLimits(map[string]any{field: uint64(math.MaxInt64) + 1})
			if err == nil || !strings.Contains(err.Error(), "limits."+field+" must be a positive integer") {
				t.Fatalf("overflow error = %v", err)
			}
		})
	}

	settings := writeSettings(t, "limits = 1\n")
	if _, err := LoadRuntimeConfig(settings); err == nil || !strings.Contains(err.Error(), "limits must be a table") {
		t.Fatalf("non-table limits error = %v", err)
	}
	settings = writeSettings(t, "[limits]\nunknown_ceiling = 1\n")
	if _, err := LoadRuntimeConfig(settings); err == nil || !strings.Contains(err.Error(), `limits contains unknown field "unknown_ceiling"`) {
		t.Fatalf("unknown limit error = %v", err)
	}
	settings = writeSettings(t, "[limits]\nnamed_input_max_bytes = 5\nnamed_input_total_max_bytes = 4\n")
	if _, err := LoadRuntimeConfig(settings); err == nil ||
		!strings.Contains(err.Error(), "named_input_total_max_bytes must be greater than or equal to limits.named_input_max_bytes") {
		t.Fatalf("aggregate relationship error = %v", err)
	}
}

func TestApplyTransientRecipeSourcesPreservesRuntimeLimits(t *testing.T) {
	config, err := LoadRuntimeConfig(writeSettings(t, `[limits]
integration_bundle_max_bytes = 2048
named_input_max_bytes = 1024
named_input_total_max_bytes = 4096
repository_inventory_max_files = 33
repository_inventory_max_bytes = 8192
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	applied, _, err := ApplyTransientRecipeSources(config, nil)
	if err != nil {
		t.Fatalf("apply transient sources: %v", err)
	}
	if applied.Limits != (RuntimeLimits{
		IntegrationBundleMaxBytes:   2048,
		NamedInputMaxBytes:          1024,
		NamedInputTotalMaxBytes:     4096,
		RepositoryInventoryMaxFiles: 33,
		RepositoryInventoryMaxBytes: 8192,
	}) {
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
	effective := custom.EffectiveLimits()
	if effective.IntegrationBundleMaxBytes != 4096 ||
		effective.NamedInputMaxBytes != DefaultNamedInputMaxBytes ||
		effective.NamedInputTotalMaxBytes != DefaultNamedInputTotalMaxBytes ||
		effective.RepositoryInventoryMaxFiles != DefaultRepositoryInventoryMaxFiles ||
		effective.RepositoryInventoryMaxBytes != DefaultRepositoryInventoryMaxBytes {
		t.Fatalf("custom effective limits = %#v", effective)
	}
	if RuntimeLimitsProvided(RuntimeLimits{}) {
		t.Fatal("zero limits unexpectedly reported as provided")
	}
	if !RuntimeLimitsProvided(RuntimeLimits{RepositoryInventoryMaxFiles: 1}) {
		t.Fatal("single new nonzero limit was not reported as provided")
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
	want := RuntimeLimitsMap(DefaultRuntimeLimits())
	for key, value := range want {
		if limits[key] != float64(value.(int64)) {
			t.Fatalf("runtime config limits JSON[%s] = %#v, want %v", key, limits[key], value)
		}
	}
	if len(limits) != len(want) {
		t.Fatalf("runtime config limits JSON = %#v", limits)
	}
}
