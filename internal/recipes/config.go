package recipes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/pelletier/go-toml/v2"
)

const (
	SettingsEnvVar                       = "CONVO_RELAY_SETTINGS"
	TransientRecipeSourceMaxBytes  int64 = 64 * 1024
	TransientRecipeSourceOrdinary        = "ordinary"
	TransientRecipeSourceGenerated       = "generated"
)

type RuntimeConfig struct {
	BackendProfiles map[string]map[string]any `json:"backend_profiles"`
	RelayRecipes    map[string]map[string]any `json:"relay_recipes"`
	Limits          RuntimeLimits             `json:"limits"`
	SettingsPath    string                    `json:"settings_path,omitempty"`
}

type TransientRecipeSource struct {
	SourceType    string            `json:"source_type"`
	Path          string            `json:"path"`
	DisplayName   string            `json:"display_name"`
	Digest        string            `json:"digest"`
	SourceDigest  string            `json:"source_digest"`
	SizeBytes     int64             `json:"size_bytes"`
	Content       string            `json:"content"`
	RawTOML       []byte            `json:"-"`
	RecipeIDs     []string          `json:"recipe_ids"`
	RecipeDigests map[string]string `json:"recipe_digests,omitempty"`
	ProfileIDs    []string          `json:"profile_ids,omitempty"`
}

type TransientRecipeFile = TransientRecipeSource

func LoadRuntimeConfig(settingsPath string) (RuntimeConfig, error) {
	config, _, err := LoadRuntimeConfigWithTransientFiles(settingsPath, nil)
	return config, err
}

func LoadRuntimeConfigWithTransientFiles(settingsPath string, recipeFiles []string) (RuntimeConfig, []TransientRecipeFile, error) {
	sources, err := ReadTransientRecipeSources(recipeFiles, nil, nil)
	if err != nil {
		return RuntimeConfig{}, nil, err
	}
	return LoadRuntimeConfigWithTransientSources(settingsPath, sources)
}

func LoadRuntimeConfigWithTransientSources(settingsPath string, sources []TransientRecipeSource) (RuntimeConfig, []TransientRecipeSource, error) {
	path := resolveSettingsPath(settingsPath)
	settings, err := loadTOML(path)
	if err != nil {
		return RuntimeConfig{}, nil, err
	}
	rawProfiles := asObject(settings["backend_profiles"])
	rawRecipes := asObject(settings["relay_recipes"])
	limits, err := ParseRuntimeLimits(settings["limits"])
	if err != nil {
		return RuntimeConfig{}, nil, err
	}
	transientFiles, err := loadTransientRecipeSources(sources, rawProfiles, rawRecipes)
	if err != nil {
		return RuntimeConfig{}, nil, err
	}
	normalizedProfiles := NormalizeBackendProfiles(rawProfiles)
	normalizedRecipes := NormalizeRelayRecipes(rawRecipes)
	if err := validateTransientRecipes(transientFiles, normalizedProfiles, normalizedRecipes); err != nil {
		return RuntimeConfig{}, nil, err
	}
	return RuntimeConfig{
		BackendProfiles: normalizedProfiles,
		RelayRecipes:    normalizedRecipes,
		Limits:          limits,
		SettingsPath:    path,
	}, transientFiles, nil
}

func ApplyTransientRecipeSources(base RuntimeConfig, sources []TransientRecipeSource) (RuntimeConfig, []TransientRecipeSource, error) {
	if err := ValidateRuntimeLimits(base.EffectiveLimits()); err != nil {
		return RuntimeConfig{}, nil, err
	}
	rawProfiles := runtimeMapAsRaw(base.BackendProfiles)
	rawRecipes := runtimeMapAsRaw(base.RelayRecipes)
	transientFiles, err := loadTransientRecipeSources(sources, rawProfiles, rawRecipes)
	if err != nil {
		return RuntimeConfig{}, nil, err
	}
	normalizedProfiles := NormalizeBackendProfiles(rawProfiles)
	normalizedRecipes := NormalizeRelayRecipes(rawRecipes)
	if err := validateTransientRecipes(transientFiles, normalizedProfiles, normalizedRecipes); err != nil {
		return RuntimeConfig{}, nil, err
	}
	return RuntimeConfig{
		BackendProfiles: normalizedProfiles,
		RelayRecipes:    normalizedRecipes,
		Limits:          base.EffectiveLimits(),
		SettingsPath:    strings.TrimSpace(base.SettingsPath),
	}, transientFiles, nil
}

func ReadTransientRecipeSources(recipeFiles []string, generatedRecipeFiles []string, stdin io.Reader) ([]TransientRecipeSource, error) {
	sourceSpecs := make([]TransientRecipeSource, 0, len(recipeFiles)+len(generatedRecipeFiles))
	for _, path := range recipeFiles {
		sourceSpecs = append(sourceSpecs, TransientRecipeSource{SourceType: TransientRecipeSourceOrdinary, Path: path})
	}
	for _, path := range generatedRecipeFiles {
		sourceSpecs = append(sourceSpecs, TransientRecipeSource{SourceType: TransientRecipeSourceGenerated, Path: path})
	}
	if len(sourceSpecs) == 0 {
		return nil, nil
	}
	sources := make([]TransientRecipeSource, 0, len(sourceSpecs))
	seenPaths := map[string]bool{}
	stdinUsed := false
	for _, spec := range sourceSpecs {
		sourceType := normalizeTransientRecipeSourceType(spec.SourceType)
		path := strings.TrimSpace(spec.Path)
		if path == "" {
			return nil, fmt.Errorf("--recipe-file path is empty")
		}
		if path == "-" {
			if stdin == nil {
				return nil, fmt.Errorf("recipe source - requires stdin")
			}
			if stdinUsed {
				return nil, fmt.Errorf("stdin recipe source can only be used once")
			}
			stdinUsed = true
			data, err := io.ReadAll(io.LimitReader(stdin, TransientRecipeSourceMaxBytes+1))
			if err != nil {
				return nil, fmt.Errorf("read recipe source -: %w", err)
			}
			if int64(len(data)) > TransientRecipeSourceMaxBytes {
				return nil, fmt.Errorf("recipe source - exceeds %d bytes", TransientRecipeSourceMaxBytes)
			}
			sources = append(sources, transientRecipeSourceFromBytes(sourceType, "-", "stdin", data))
			continue
		}
		absPath, err := filepath.Abs(expandUser(path))
		if err != nil {
			return nil, fmt.Errorf("recipe file %q: %w", spec.Path, err)
		}
		normalizedPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return nil, fmt.Errorf("recipe file %q cannot be normalized: %w", spec.Path, err)
		}
		if seenPaths[normalizedPath] {
			return nil, fmt.Errorf("duplicate recipe file after path normalization: %s", spec.Path)
		}
		seenPaths[normalizedPath] = true
		info, err := os.Stat(normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("recipe file %q is unreadable: %w", spec.Path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("recipe file %q is a directory; provide a TOML file", spec.Path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recipe file %q is not a regular file", spec.Path)
		}
		if info.Size() > TransientRecipeSourceMaxBytes {
			return nil, fmt.Errorf("recipe file %q exceeds %d bytes", spec.Path, TransientRecipeSourceMaxBytes)
		}
		data, err := os.ReadFile(normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("recipe file %q is unreadable: %w", spec.Path, err)
		}
		if int64(len(data)) > TransientRecipeSourceMaxBytes {
			return nil, fmt.Errorf("recipe file %q exceeds %d bytes", spec.Path, TransientRecipeSourceMaxBytes)
		}
		sources = append(sources, transientRecipeSourceFromBytes(sourceType, normalizedPath, filepath.Base(normalizedPath), data))
	}
	return sources, nil
}

func NormalizeBackendProfiles(rawProfiles map[string]any) map[string]map[string]any {
	profiles := mergeNamedRecords(defaultBackendProfiles, rawProfiles)
	normalized := map[string]map[string]any{}
	for _, profileID := range sortedObjectKeys(profiles) {
		profile := profiles[profileID]
		backend := strings.TrimSpace(stringValue(profile["backend"]))
		if !backendRegistry[backend] {
			continue
		}
		normalized[profileID] = map[string]any{
			"id":           profileID,
			"backend":      backend,
			"model":        profile["model"],
			"effort":       profile["effort"],
			"description":  strings.TrimSpace(stringValue(profile["description"])),
			"capabilities": cleanStringList(profile["capabilities"], false),
		}
		if strings.TrimSpace(stringValue(profile["origin"])) == "generated" {
			normalized[profileID]["origin"] = "generated"
			if generatedFromRef := strings.TrimSpace(stringValue(profile["generated_from_ref"])); generatedFromRef != "" {
				normalized[profileID]["generated_from_ref"] = generatedFromRef
			}
			if generatedSource := strings.TrimSpace(stringValue(profile["generated_source"])); generatedSource != "" {
				normalized[profileID]["generated_source"] = generatedSource
			}
			if generatedProfileID := strings.TrimSpace(stringValue(profile["generated_profile_id"])); generatedProfileID != "" {
				normalized[profileID]["generated_profile_id"] = generatedProfileID
			}
		}
	}
	return normalized
}

func NormalizeRelayRecipes(rawRecipes map[string]any) map[string]map[string]any {
	recipes := mergeNamedRecords(defaultRelayRecipes, rawRecipes)
	normalized := map[string]map[string]any{}
	for _, recipeID := range sortedObjectKeys(recipes) {
		recipe := recipes[recipeID]
		participants := cleanStringList(recipe["participants"], false)
		if len(participants) != 2 {
			continue
		}
		facilitator := strings.TrimSpace(stringValue(recipe["facilitator"]))
		if facilitator == "" {
			facilitator = participants[0].(string)
		}
		reducer := strings.TrimSpace(stringValue(recipe["reducer"]))
		if reducer == "" {
			reducer = facilitator
		}
		normalized[recipeID] = normalizeRecipePayload(map[string]any{
			"id":                    recipeID,
			"purpose":               strings.TrimSpace(stringValue(recipe["purpose"])),
			"participants":          participants,
			"facilitator":           facilitator,
			"reducer":               reducer,
			"mode":                  normalizeMode(recipe["mode"]),
			"max_rounds":            positiveInt(recipe["max_rounds"], 1),
			"max_depth":             positiveInt(recipe["max_depth"], 1),
			"required_capabilities": cleanStringList(recipe["required_capabilities"], false),
			"auto_approval":         normalizeAutoApproval(recipe["auto_approval"]),
			"match_keywords":        cleanStringList(recipe["match_keywords"], true),
			"origin":                recipe["origin"],
			"generated_from_ref":    recipe["generated_from_ref"],
			"generated_source":      recipe["generated_source"],
			"generated_recipe_id":   recipe["generated_recipe_id"],
		})
	}
	return normalized
}

func resolveSettingsPath(settingsPath string) string {
	if strings.TrimSpace(settingsPath) != "" {
		return expandUser(settingsPath)
	}
	if envPath := os.Getenv(SettingsEnvVar); strings.TrimSpace(envPath) != "" {
		return expandUser(envPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".convo-relay", "settings.toml")
	}
	return filepath.Join(home, ".convo-relay", "settings.toml")
}

func loadTOML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return decodeTOMLBytes(data)
}

func decodeTOMLBytes(data []byte) (map[string]any, error) {
	var settings map[string]any
	if err := toml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	return materializeMap(settings), nil
}

func loadTransientRecipeSources(sources []TransientRecipeSource, rawProfiles map[string]any, rawRecipes map[string]any) ([]TransientRecipeSource, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	profileCounts, recipeCounts, err := transientSourceIDCounts(sources)
	if err != nil {
		return nil, err
	}
	files := make([]TransientRecipeSource, 0, len(sources))
	generatedProfileIDs := map[string]bool{}
	generatedRecipeIDs := map[string]bool{}
	for _, source := range sources {
		source.SourceType = normalizeTransientRecipeSourceType(source.SourceType)
		data := source.RawTOML
		if data == nil {
			data = []byte(source.Content)
		}
		if int64(len(data)) > TransientRecipeSourceMaxBytes {
			return nil, fmt.Errorf("recipe source %s exceeds %d bytes", source.Path, TransientRecipeSourceMaxBytes)
		}
		parsed, err := decodeTOMLBytes(data)
		if err != nil {
			return nil, fmt.Errorf("load recipe source %s: %w", source.Path, err)
		}
		profiles := asObject(parsed["backend_profiles"])
		recipes := asObject(parsed["relay_recipes"])
		if len(recipes) == 0 {
			return nil, fmt.Errorf("recipe source %s must define at least one [relay_recipes.<id>] table", source.Path)
		}
		profileIDs := sortedObjectKeys(profiles)
		recipeIDs := sortedObjectKeys(recipes)
		if source.SourceType == TransientRecipeSourceGenerated {
			if err := validateGeneratedSourceCollisions(source, profileIDs, recipeIDs, rawProfiles, rawRecipes, generatedProfileIDs, generatedRecipeIDs, profileCounts, recipeCounts); err != nil {
				return nil, err
			}
			applyGeneratedProfileOverrides(source, profiles)
			applyGeneratedRecipeOverrides(source, recipes)
		}
		for id, profile := range profiles {
			rawProfiles[id] = profile
		}
		for id, recipe := range recipes {
			rawRecipes[id] = recipe
		}
		if source.SourceType == TransientRecipeSourceGenerated {
			for _, id := range profileIDs {
				generatedProfileIDs[id] = true
			}
			for _, id := range recipeIDs {
				generatedRecipeIDs[id] = true
			}
		}
		if source.Digest == "" {
			source.Digest = digestBytes(data)
		}
		if source.SourceDigest == "" {
			source.SourceDigest = source.Digest
		}
		source.SizeBytes = int64(len(data))
		source.Content = string(data)
		source.RawTOML = append([]byte(nil), data...)
		source.RecipeIDs = recipeIDs
		source.RecipeDigests = recipeDigestsForIDs(recipeIDs, NormalizeRelayRecipes(rawRecipes))
		source.ProfileIDs = profileIDs
		files = append(files, source)
	}
	return files, nil
}

func transientSourceIDCounts(sources []TransientRecipeSource) (map[string]int, map[string]int, error) {
	profileCounts := map[string]int{}
	recipeCounts := map[string]int{}
	for _, source := range sources {
		data := source.RawTOML
		if data == nil {
			data = []byte(source.Content)
		}
		if int64(len(data)) > TransientRecipeSourceMaxBytes {
			return nil, nil, fmt.Errorf("recipe source %s exceeds %d bytes", source.Path, TransientRecipeSourceMaxBytes)
		}
		parsed, err := decodeTOMLBytes(data)
		if err != nil {
			return nil, nil, fmt.Errorf("load recipe source %s: %w", source.Path, err)
		}
		profiles := asObject(parsed["backend_profiles"])
		recipes := asObject(parsed["relay_recipes"])
		if len(recipes) == 0 {
			return nil, nil, fmt.Errorf("recipe source %s must define at least one [relay_recipes.<id>] table", source.Path)
		}
		for _, id := range sortedObjectKeys(profiles) {
			profileCounts[id]++
		}
		for _, id := range sortedObjectKeys(recipes) {
			recipeCounts[id]++
		}
	}
	return profileCounts, recipeCounts, nil
}

func transientRecipeSourceFromBytes(sourceType string, path string, displayName string, data []byte) TransientRecipeSource {
	digest := digestBytes(data)
	return TransientRecipeSource{
		SourceType:   normalizeTransientRecipeSourceType(sourceType),
		Path:         path,
		DisplayName:  displayName,
		Digest:       digest,
		SourceDigest: digest,
		SizeBytes:    int64(len(data)),
		Content:      string(data),
		RawTOML:      append([]byte(nil), data...),
	}
}

func normalizeTransientRecipeSourceType(sourceType string) string {
	switch strings.TrimSpace(sourceType) {
	case TransientRecipeSourceGenerated:
		return TransientRecipeSourceGenerated
	default:
		return TransientRecipeSourceOrdinary
	}
}

func applyGeneratedRecipeOverrides(source TransientRecipeSource, recipes map[string]any) {
	for recipeID, rawRecipe := range recipes {
		recipe, ok := rawRecipe.(map[string]any)
		if !ok {
			continue
		}
		recipe["auto_approval"] = "never"
		recipe["origin"] = "generated"
		recipe["generated_from_ref"] = source.Path
		recipe["generated_source"] = strings.TrimSpace(source.DisplayName)
		recipe["generated_recipe_id"] = strings.TrimSpace(recipeID)
	}
}

func applyGeneratedProfileOverrides(source TransientRecipeSource, profiles map[string]any) {
	for profileID, rawProfile := range profiles {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			continue
		}
		profile["origin"] = "generated"
		profile["generated_from_ref"] = source.Path
		profile["generated_source"] = strings.TrimSpace(source.DisplayName)
		profile["generated_profile_id"] = strings.TrimSpace(profileID)
	}
}

func validateGeneratedSourceCollisions(source TransientRecipeSource, profileIDs []string, recipeIDs []string, rawProfiles map[string]any, rawRecipes map[string]any, generatedProfileIDs map[string]bool, generatedRecipeIDs map[string]bool, profileCounts map[string]int, recipeCounts map[string]int) error {
	for _, profileID := range profileIDs {
		if _, ok := defaultBackendProfiles[profileID]; ok || backendRegistry[profileID] || rawProfiles[profileID] != nil || generatedProfileIDs[profileID] || profileCounts[profileID] > 1 {
			return fmt.Errorf("generated recipe source %s defines backend profile %q that collides with an existing profile", source.Path, profileID)
		}
	}
	for _, recipeID := range recipeIDs {
		if _, ok := defaultRelayRecipes[recipeID]; ok || rawRecipes[recipeID] != nil || generatedRecipeIDs[recipeID] || recipeCounts[recipeID] > 1 {
			return fmt.Errorf("generated recipe source %s defines relay recipe %q that collides with an existing recipe", source.Path, recipeID)
		}
	}
	return nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type transientRecipeDigestTrace struct {
	SourceType   string
	Path         string
	DisplayName  string
	SourceDigest string
	RecipeDigest string
}

func transientRecipeDigestTraces(sources []TransientRecipeSource) map[string]transientRecipeDigestTrace {
	result := map[string]transientRecipeDigestTrace{}
	for _, source := range sources {
		sourceDigest := strings.TrimSpace(source.SourceDigest)
		if sourceDigest == "" {
			sourceDigest = strings.TrimSpace(source.Digest)
		}
		for _, recipeID := range source.RecipeIDs {
			result[recipeID] = transientRecipeDigestTrace{
				SourceType:   source.SourceType,
				Path:         source.Path,
				DisplayName:  source.DisplayName,
				SourceDigest: sourceDigest,
				RecipeDigest: strings.TrimSpace(source.RecipeDigests[recipeID]),
			}
		}
	}
	return result
}

func recipeDigestsForIDs(recipeIDs []string, normalizedRecipes map[string]map[string]any) map[string]string {
	result := map[string]string{}
	for _, recipeID := range recipeIDs {
		recipe := normalizedRecipes[recipeID]
		if recipe == nil {
			continue
		}
		digest, err := contracts.ContractDigest(RecipeContractPayload(recipe))
		if err == nil {
			result[recipeID] = digest
		}
	}
	return result
}

func runtimeMapAsRaw(values map[string]map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneObject(value)
	}
	return result
}

func validateTransientRecipes(files []TransientRecipeFile, profiles map[string]map[string]any, relayRecipes map[string]map[string]any) error {
	for _, file := range files {
		for _, recipeID := range file.RecipeIDs {
			recipe := relayRecipes[recipeID]
			if recipe == nil {
				return ChildRelayConfigError{
					Message: fmt.Sprintf("Transient recipe file %s is not executable.", file.Path),
					Issues: []ChildRecipeIssue{{
						Category: "invalid_config",
						Code:     "transient_recipe_skipped",
						Message:  fmt.Sprintf("Transient recipe '%s' was parseable but skipped by normalization.", recipeID),
						Path:     "relay_recipes." + recipeID,
						Detail:   map[string]any{"recipe_file": file.Path},
					}},
				}
			}
			if issues := ExecutableIssues(recipe, profiles, relayRecipes, DepthPolicy{}, "root"); len(issues) > 0 {
				return ChildRelayConfigError{
					Message: fmt.Sprintf("Transient recipe file %s is not executable.", file.Path),
					Issues:  issues,
				}
			}
		}
	}
	return nil
}

func expandUser(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
