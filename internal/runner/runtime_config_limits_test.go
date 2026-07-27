package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	wantLimits := recipes.RuntimeLimitsMap(config.EffectiveLimits())
	for key, value := range wantLimits {
		if fmt.Sprint(limits[key]) != fmt.Sprint(value) {
			t.Fatalf("snapshot limits[%s] = %v, want %v", key, limits[key], value)
		}
	}
	loaded, err := loadRuntimeConfigFromSnapshot(st, ref)
	if err != nil {
		t.Fatalf("load snapshot config: %v", err)
	}
	if loaded.Limits.IntegrationBundleMaxBytes != 4096 {
		t.Fatalf("loaded limits = %#v", loaded.Limits)
	}
}

func TestRuntimeConfigV1SnapshotFixtureRemainsReadableWithDefaultLimit(t *testing.T) {
	fixtureRoot, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sessions", "runtime-config-v1"))
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}
	st := store.New(fixtureRoot)
	ref, err := st.ResolveArtifactRef("artifacts/runtime_config/launch.json", "")
	if err != nil {
		t.Fatalf("resolve v1 snapshot fixture: %v", err)
	}
	path, err := st.ArtifactPathForRef(ref)
	if err != nil {
		t.Fatalf("resolve v1 snapshot path: %v", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(st.Root, path)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read v1 snapshot fixture: %v", err)
	}
	originalDigest := fmt.Sprint(ref["digest"])
	loaded, err := loadRuntimeConfigFromSnapshot(st, ref)
	if err != nil {
		t.Fatalf("load v1 snapshot: %v", err)
	}
	if loaded.Limits != recipes.DefaultRuntimeLimits() {
		t.Fatalf("v1 limits = %#v", loaded.Limits)
	}
	if loaded.BackendProfiles["fixture-profile"]["backend"] != "codex" || loaded.RelayRecipes["fixture-recipe"] == nil {
		t.Fatalf("v1 runtime config = %#v", loaded)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reread v1 snapshot fixture: %v", err)
	}
	resolvedAgain, err := st.ResolveArtifactRef("artifacts/runtime_config/launch.json", "")
	if err != nil || fmt.Sprint(resolvedAgain["digest"]) != originalDigest || !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy snapshot changed: ref=%#v err=%v bytes_equal=%v", resolvedAgain, err, reflect.DeepEqual(after, before))
	}

	explicitStore := store.New(t.TempDir())
	explicitRef, err := persistRuntimeConfigSnapshot(explicitStore, loaded)
	if err != nil {
		t.Fatalf("persist explicit-default snapshot: %v", err)
	}
	explicit, err := loadRuntimeConfigFromSnapshot(explicitStore, explicitRef)
	if err != nil {
		t.Fatalf("load explicit-default snapshot: %v", err)
	}
	if !reflect.DeepEqual(explicit, loaded) {
		t.Fatalf("legacy and explicit recovery configs differ:\nlegacy=%#v\nexplicit=%#v", loaded, explicit)
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

func TestLegacyV2SnapshotDefaultsNewLimitsWithoutRewritingStoredEvidence(t *testing.T) {
	st := store.New(t.TempDir())
	config := runtimeLimitsTestConfig(4096)
	ref, err := st.SaveArtifact("runtime_config", "legacy-v2", map[string]any{
		"version":          RuntimeConfigSnapshotVersion,
		"settings_path":    config.SettingsPath,
		"backend_profiles": config.BackendProfiles,
		"relay_recipes":    config.RelayRecipes,
		"limits": map[string]any{
			"integration_bundle_max_bytes": 4096,
		},
	})
	if err != nil {
		t.Fatalf("persist legacy v2 snapshot: %v", err)
	}
	path, err := st.ArtifactPathForRef(ref)
	if err != nil {
		t.Fatalf("legacy v2 path: %v", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(st.Root, path)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy v2 snapshot: %v", err)
	}
	beforeDigest := fmt.Sprint(ref["digest"])

	loaded, err := loadRuntimeConfigFromSnapshot(st, ref)
	if err != nil {
		t.Fatalf("load legacy v2 snapshot: %v", err)
	}
	if loaded.Limits.IntegrationBundleMaxBytes != 4096 ||
		loaded.Limits.NamedInputMaxBytes != recipes.DefaultNamedInputMaxBytes ||
		loaded.Limits.NamedInputTotalMaxBytes != recipes.DefaultNamedInputTotalMaxBytes ||
		loaded.Limits.RepositoryInventoryMaxFiles != recipes.DefaultRepositoryInventoryMaxFiles ||
		loaded.Limits.RepositoryInventoryMaxBytes != recipes.DefaultRepositoryInventoryMaxBytes {
		t.Fatalf("legacy v2 effective limits = %#v", loaded.Limits)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reread legacy v2 snapshot: %v", err)
	}
	resolved, err := st.ResolveArtifactRef(fmt.Sprint(ref["id"]), beforeDigest)
	if err != nil || string(after) != string(before) || fmt.Sprint(resolved["digest"]) != beforeDigest {
		t.Fatalf("legacy v2 evidence changed: ref=%#v err=%v bytes_equal=%v", resolved, err, string(after) == string(before))
	}
}

func TestRuntimeLimitsPropagateThroughCloneAndGraphFallback(t *testing.T) {
	config := runtimeLimitsTestConfig(8192)
	config.Limits = recipes.RuntimeLimits{
		IntegrationBundleMaxBytes:   8192,
		NamedInputMaxBytes:          1024,
		NamedInputTotalMaxBytes:     4096,
		RepositoryInventoryMaxFiles: 55,
		RepositoryInventoryMaxBytes: 16384,
	}
	cloned := cloneRuntimeConfig(config)
	if cloned.Limits != config.Limits {
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
	if graphConfig.Limits.IntegrationBundleMaxBytes != 16384 ||
		graphConfig.Limits.NamedInputMaxBytes != recipes.DefaultNamedInputMaxBytes ||
		graphConfig.Limits.NamedInputTotalMaxBytes != recipes.DefaultNamedInputTotalMaxBytes ||
		graphConfig.Limits.RepositoryInventoryMaxFiles != recipes.DefaultRepositoryInventoryMaxFiles ||
		graphConfig.Limits.RepositoryInventoryMaxBytes != recipes.DefaultRepositoryInventoryMaxBytes {
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

func TestDirectRuntimeConfigRecognizesSingleNewLimit(t *testing.T) {
	provided := recipes.RuntimeConfig{
		Limits: recipes.RuntimeLimits{RepositoryInventoryMaxFiles: 7},
	}
	loaded, _, err := loadEffectiveRuntimeConfig("", provided, nil, nil, nil)
	if err != nil {
		t.Fatalf("single-limit direct runtime config: %v", err)
	}
	if loaded.Limits.RepositoryInventoryMaxFiles != 7 ||
		loaded.Limits.NamedInputMaxBytes != recipes.DefaultNamedInputMaxBytes {
		t.Fatalf("single-limit direct runtime config was discarded: %#v", loaded.Limits)
	}

	provided.Limits.RepositoryInventoryMaxFiles = -1
	if _, _, err := loadEffectiveRuntimeConfig("", provided, nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "repository_inventory_max_files") {
		t.Fatalf("single invalid new limit was not validated: %v", err)
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
