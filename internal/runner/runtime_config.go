package runner

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const (
	RuntimeConfigSnapshotVersion   = "runtime-config/v2"
	RuntimeConfigSnapshotVersionV1 = "runtime-config/v1"
)

func loadEffectiveRuntimeConfig(settingsPath string, provided recipes.RuntimeConfig, recipeFiles []string, generatedRecipeFiles []string, transientSources []recipes.TransientRecipeSource) (recipes.RuntimeConfig, []recipes.TransientRecipeFile, error) {
	if runtimeConfigProvided(provided) {
		if err := recipes.ValidateRuntimeLimits(provided.EffectiveLimits()); err != nil {
			return recipes.RuntimeConfig{}, nil, err
		}
		config := cloneRuntimeConfig(provided)
		if strings.TrimSpace(config.SettingsPath) == "" {
			config.SettingsPath = strings.TrimSpace(settingsPath)
		}
		return config, nil, nil
	}
	sources := append([]recipes.TransientRecipeSource{}, transientSources...)
	if len(recipeFiles) > 0 || len(generatedRecipeFiles) > 0 {
		fileSources, err := recipes.ReadTransientRecipeSources(recipeFiles, generatedRecipeFiles, nil)
		if err != nil {
			return recipes.RuntimeConfig{}, nil, err
		}
		sources = append(sources, fileSources...)
	}
	return recipes.LoadRuntimeConfigWithTransientSources(settingsPath, sources)
}

func runtimeConfigProvided(config recipes.RuntimeConfig) bool {
	return len(config.BackendProfiles) > 0 && len(config.RelayRecipes) > 0
}

func cloneRuntimeConfig(config recipes.RuntimeConfig) recipes.RuntimeConfig {
	return recipes.RuntimeConfig{
		BackendProfiles: mapStringObjectMap(config.BackendProfiles),
		RelayRecipes:    mapStringObjectMap(config.RelayRecipes),
		Limits:          config.EffectiveLimits(),
		SettingsPath:    strings.TrimSpace(config.SettingsPath),
	}
}

func runtimeConfigSnapshotPayload(config recipes.RuntimeConfig) map[string]any {
	return map[string]any{
		"version":          RuntimeConfigSnapshotVersion,
		"settings_path":    strings.TrimSpace(config.SettingsPath),
		"backend_profiles": mapStringObjectMap(config.BackendProfiles),
		"relay_recipes":    mapStringObjectMap(config.RelayRecipes),
		"limits":           recipes.RuntimeLimitsMap(config.EffectiveLimits()),
	}
}

func prepareRuntimeConfigSnapshot(config recipes.RuntimeConfig) (map[string]any, error) {
	body, err := contracts.CanonicalJSONBytes(runtimeConfigSnapshotPayload(config))
	if err != nil {
		return nil, fmt.Errorf("runtime config snapshot is not persistable JSON: %w", err)
	}
	snapshot, err := contracts.DecodeJSONObjectBytes(body)
	if err != nil {
		return nil, fmt.Errorf("normalize runtime config snapshot: %w", err)
	}
	return snapshot, nil
}

func persistPreparedRuntimeConfigSnapshot(st *store.Store, snapshot map[string]any) (map[string]any, error) {
	return st.SaveArtifact("runtime_config", "launch", snapshot)
}

func persistRuntimeConfigSnapshot(st *store.Store, config recipes.RuntimeConfig) (map[string]any, error) {
	snapshot, err := prepareRuntimeConfigSnapshot(config)
	if err != nil {
		return nil, err
	}
	return persistPreparedRuntimeConfigSnapshot(st, snapshot)
}

func loadRuntimeConfigFromSnapshot(st *store.Store, ref any) (recipes.RuntimeConfig, error) {
	refMap, ok := ref.(map[string]any)
	if !ok || refMap == nil {
		return recipes.RuntimeConfig{}, fmt.Errorf("runtime_config_ref is missing or invalid")
	}
	payload, err := st.LoadArtifact(refMap)
	if err != nil {
		return recipes.RuntimeConfig{}, err
	}
	version := strings.TrimSpace(stringFromAny(payload["version"]))
	if version != RuntimeConfigSnapshotVersion && version != RuntimeConfigSnapshotVersionV1 {
		return recipes.RuntimeConfig{}, fmt.Errorf("runtime config snapshot version %q is unsupported", version)
	}
	limits := recipes.DefaultRuntimeLimits()
	if version == RuntimeConfigSnapshotVersion {
		var exists bool
		var err error
		rawLimits, exists := payload["limits"]
		if !exists {
			return recipes.RuntimeConfig{}, fmt.Errorf("runtime config v2 snapshot is missing limits")
		}
		limits, err = recipes.ParseRuntimeLimits(rawLimits)
		if err != nil {
			return recipes.RuntimeConfig{}, fmt.Errorf("runtime config v2 snapshot: %w", err)
		}
	}
	config := recipes.RuntimeConfig{
		BackendProfiles: mapStringObjectMap(payload["backend_profiles"]),
		RelayRecipes:    mapStringObjectMap(payload["relay_recipes"]),
		Limits:          limits,
		SettingsPath:    strings.TrimSpace(stringFromAny(payload["settings_path"])),
	}
	if !runtimeConfigProvided(config) {
		return recipes.RuntimeConfig{}, fmt.Errorf("runtime config snapshot does not contain backend profiles or relay recipes")
	}
	return config, nil
}

func effectiveRuntimeConfigForSession(st *store.Store, meta map[string]any, settingsPathOverride string) (recipes.RuntimeConfig, string, error) {
	if ref, ok := meta["runtime_config_ref"].(map[string]any); ok && ref != nil {
		config, err := loadRuntimeConfigFromSnapshot(st, ref)
		if err != nil {
			return recipes.RuntimeConfig{}, "", err
		}
		return config, config.SettingsPath, nil
	}
	graphConfig, err := runtimeConfigFromGraph(st.LoadGraph(), stringFromAny(meta["settings_path"]))
	if err != nil {
		return recipes.RuntimeConfig{}, "", err
	}
	if runtimeConfigProvided(graphConfig) {
		return graphConfig, graphConfig.SettingsPath, nil
	}
	settingsPath := firstNonEmpty(settingsPathOverride, stringFromAny(meta["settings_path"]))
	config, err := recipes.LoadRuntimeConfig(settingsPath)
	if err != nil {
		return recipes.RuntimeConfig{}, "", err
	}
	return config, config.SettingsPath, nil
}

func effectiveRuntimeConfigForResume(st *store.Store, meta map[string]any, opts ResumeOptions) (recipes.RuntimeConfig, string, error) {
	if ref, ok := meta["runtime_config_ref"].(map[string]any); ok && ref != nil {
		if strings.TrimSpace(opts.SettingsPath) != "" {
			return recipes.RuntimeConfig{}, "", fmt.Errorf("resume --settings is not supported for sessions with a runtime config snapshot; start a new relay instead")
		}
		config, err := loadRuntimeConfigFromSnapshot(st, ref)
		if err != nil {
			return recipes.RuntimeConfig{}, "", err
		}
		return config, config.SettingsPath, nil
	}
	settingsPath := opts.SettingsPath
	if settingsPath == "" {
		settingsPath = stringFromAny(meta["settings_path"])
	}
	config, resolvedPath, err := effectiveRuntimeConfigForSession(st, meta, settingsPath)
	if err != nil {
		return recipes.RuntimeConfig{}, "", err
	}
	return config, resolvedPath, nil
}

func runtimeConfigFromGraph(graphPayload map[string]any, settingsPath string) (recipes.RuntimeConfig, error) {
	limits, err := recipes.ParseRuntimeLimits(graphPayload["runtime_limits"])
	if err != nil {
		return recipes.RuntimeConfig{}, fmt.Errorf("graph runtime limits: %w", err)
	}
	return recipes.RuntimeConfig{
		BackendProfiles: mapStringObjectMap(graphPayload["backend_profiles"]),
		RelayRecipes:    mapStringObjectMap(graphPayload["relay_recipes"]),
		Limits:          limits,
		SettingsPath:    strings.TrimSpace(settingsPath),
	}, nil
}

func mutateSessionRuntimeConfig(sessionDir string, sources []recipes.TransientRecipeSource) (map[string]any, error) {
	lock, err := lockSessionMutation(sessionDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = lock.Unlock()
	}()
	if err := ensureSessionNotRunning(sessionDir, "mutating runtime config"); err != nil {
		return nil, err
	}
	st := store.New(sessionDir)
	meta, err := loadMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	currentConfig, _, err := effectiveRuntimeConfigForSession(st, meta, "")
	if err != nil {
		return nil, err
	}
	nextConfig, transientSources, err := recipes.ApplyTransientRecipeSources(currentConfig, sources)
	if err != nil {
		return nil, err
	}
	runtimeConfigRef, err := persistRuntimeConfigSnapshot(st, nextConfig)
	if err != nil {
		return nil, err
	}
	meta["runtime_config_ref"] = runtimeConfigRef
	meta["runtime_config_version"] = RuntimeConfigSnapshotVersion
	if err := store.New(sessionDir).SaveMetaMap(meta); err != nil {
		return nil, err
	}
	graphPayload := st.LoadGraph()
	graphPayload["backend_profiles"] = nextConfig.BackendProfiles
	graphPayload["relay_recipes"] = nextConfig.RelayRecipes
	graphPayload["runtime_limits"] = recipes.RuntimeLimitsMap(nextConfig.EffectiveLimits())
	graphPayload["runtime_config_ref"] = runtimeConfigRef
	if err := st.SaveGraph(graphPayload); err != nil {
		return nil, err
	}
	return map[string]any{
		"session_id":             st.SessionID(),
		"runtime_config_ref":     runtimeConfigRef,
		"transient_recipe_count": len(transientSources),
		"recipe_ids":             transientRecipeIDs(transientSources),
		"profile_ids":            transientProfileIDs(transientSources),
	}, nil
}

func transientRecipeIDs(sources []recipes.TransientRecipeSource) []any {
	items := []any{}
	for _, source := range sources {
		for _, id := range source.RecipeIDs {
			items = append(items, id)
		}
	}
	return items
}

func transientProfileIDs(sources []recipes.TransientRecipeSource) []any {
	items := []any{}
	for _, source := range sources {
		for _, id := range source.ProfileIDs {
			items = append(items, id)
		}
	}
	return items
}
