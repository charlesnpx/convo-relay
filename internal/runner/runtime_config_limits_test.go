package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRuntimeConfigV2SnapshotRoundTripPreservesLimits(t *testing.T) {
	st := store.New(t.TempDir())
	config := runtimeLimitsTestConfig(4096)
	ref, err := persistRuntimeConfigSnapshot(st, config)
	if err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	payload, err := st.LoadArtifact(ref)
	if err != nil {
		t.Fatalf("load snapshot payload: %v", err)
	}
	if payload["version"] != RuntimeConfigSnapshotVersion {
		t.Fatalf("snapshot version = %#v", payload["version"])
	}
	limits := payload["limits"].(map[string]any)
	if fmt.Sprint(limits["integration_bundle_max_bytes"]) != "4096" {
		t.Fatalf("snapshot limits = %#v", limits)
	}
	loaded, err := loadRuntimeConfigFromSnapshot(st, ref)
	if err != nil {
		t.Fatalf("load snapshot config: %v", err)
	}
	if loaded.Limits.IntegrationBundleMaxBytes != 4096 {
		t.Fatalf("loaded limits = %#v", loaded.Limits)
	}
}

func TestRuntimeConfigV1SnapshotRemainsReadableWithDefaultLimit(t *testing.T) {
	st := store.New(t.TempDir())
	config := runtimeLimitsTestConfig(0)
	ref, err := st.SaveArtifact("runtime_config", "legacy-v1", map[string]any{
		"version":          RuntimeConfigSnapshotVersionV1,
		"settings_path":    config.SettingsPath,
		"backend_profiles": config.BackendProfiles,
		"relay_recipes":    config.RelayRecipes,
	})
	if err != nil {
		t.Fatalf("persist v1 snapshot: %v", err)
	}
	loaded, err := loadRuntimeConfigFromSnapshot(st, ref)
	if err != nil {
		t.Fatalf("load v1 snapshot: %v", err)
	}
	if loaded.Limits != recipes.DefaultRuntimeLimits() {
		t.Fatalf("v1 limits = %#v", loaded.Limits)
	}
}

func TestRuntimeConfigV2SnapshotRejectsMissingOrInvalidLimits(t *testing.T) {
	tests := map[string]any{
		"missing":    nil,
		"zero":       map[string]any{"integration_bundle_max_bytes": 0},
		"fractional": map[string]any{"integration_bundle_max_bytes": 1.5},
	}
	for name, limits := range tests {
		t.Run(name, func(t *testing.T) {
			st := store.New(t.TempDir())
			config := runtimeLimitsTestConfig(0)
			payload := map[string]any{
				"version":          RuntimeConfigSnapshotVersion,
				"backend_profiles": config.BackendProfiles,
				"relay_recipes":    config.RelayRecipes,
			}
			if name != "missing" {
				payload["limits"] = limits
			}
			ref, err := st.SaveArtifact("runtime_config", name, payload)
			if err != nil {
				t.Fatalf("persist invalid snapshot: %v", err)
			}
			_, err = loadRuntimeConfigFromSnapshot(st, ref)
			if err == nil || !strings.Contains(err.Error(), "runtime config v2 snapshot") {
				t.Fatalf("load error = %v", err)
			}
		})
	}
}

func TestRuntimeLimitsPropagateThroughCloneAndGraphFallback(t *testing.T) {
	config := runtimeLimitsTestConfig(8192)
	cloned := cloneRuntimeConfig(config)
	if cloned.Limits.IntegrationBundleMaxBytes != 8192 {
		t.Fatalf("cloned limits = %#v", cloned.Limits)
	}

	graphConfig, err := runtimeConfigFromGraph(map[string]any{
		"backend_profiles": config.BackendProfiles,
		"relay_recipes":    config.RelayRecipes,
		"runtime_limits": map[string]any{
			"integration_bundle_max_bytes": 16384,
		},
	}, "settings.toml")
	if err != nil {
		t.Fatalf("graph config: %v", err)
	}
	if graphConfig.Limits.IntegrationBundleMaxBytes != 16384 {
		t.Fatalf("graph limits = %#v", graphConfig.Limits)
	}

	legacyGraphConfig, err := runtimeConfigFromGraph(map[string]any{
		"backend_profiles": config.BackendProfiles,
		"relay_recipes":    config.RelayRecipes,
	}, "settings.toml")
	if err != nil {
		t.Fatalf("legacy graph config: %v", err)
	}
	if legacyGraphConfig.Limits != recipes.DefaultRuntimeLimits() {
		t.Fatalf("legacy graph limits = %#v", legacyGraphConfig.Limits)
	}

	if _, err := runtimeConfigFromGraph(map[string]any{
		"runtime_limits": map[string]any{"integration_bundle_max_bytes": -1},
	}, ""); err == nil {
		t.Fatal("invalid graph limits unexpectedly accepted")
	}
}

func TestProvidedRuntimeConfigRejectsNegativeLimit(t *testing.T) {
	config := runtimeLimitsTestConfig(-1)
	if _, _, err := loadEffectiveRuntimeConfig("", config, nil, nil, nil); err == nil {
		t.Fatal("negative provided runtime limit unexpectedly accepted")
	}
}

func runtimeLimitsTestConfig(limit int64) recipes.RuntimeConfig {
	return recipes.RuntimeConfig{
		BackendProfiles: map[string]map[string]any{
			"test-profile": {"backend": "codex"},
		},
		RelayRecipes: map[string]map[string]any{
			"test-recipe": {"participants": []any{"test-profile", "test-profile"}},
		},
		Limits:       recipes.RuntimeLimits{IntegrationBundleMaxBytes: limit},
		SettingsPath: "settings.toml",
	}
}
