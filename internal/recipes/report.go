package recipes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/format"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/readiness"
)

const (
	RecipeStatusUsable              = "usable"
	RecipeStatusRequiresIntegration = "requires_integration"
	RecipeStatusUnavailable         = "unavailable"
	RecipeStatusInvalid             = "invalid"
	RecipeStatusSkipped             = "skipped"

	RecipeIntegrationStatusRequired = "required"
	RecipeIntegrationStatusBound    = "bound"
)

type RecipeCatalogOptions struct {
	Context           context.Context
	TransientSources  []TransientRecipeSource
	IntegrationBundle *integration.Bundle
	ReadinessOptions  readiness.Options
	ReadinessCheck    func(context.Context, []string, readiness.Options) ([]readiness.Record, error)
}

type RecipeCatalogReport struct {
	Scope        string             `json:"scope"`
	Status       string             `json:"status"`
	SettingsPath string             `json:"settings_path"`
	Recipes      []RecipeRecord     `json:"recipes"`
	Diagnostics  []ChildRecipeIssue `json:"diagnostics,omitempty"`
	IssueGroups  []RecipeIssueGroup `json:"issue_groups,omitempty"`
}

type RecipeRecord struct {
	ID               string                    `json:"id"`
	Status           string                    `json:"status"`
	Source           string                    `json:"source"`
	SourceDigest     string                    `json:"source_digest,omitempty"`
	RecipeDigest     string                    `json:"recipe_digest,omitempty"`
	Declared         map[string]any            `json:"declared,omitempty"`
	Resolved         map[string]any            `json:"resolved,omitempty"`
	Integration      *RecipeIntegrationBinding `json:"integration,omitempty"`
	BackendReadiness []readiness.Record        `json:"backend_readiness,omitempty"`
	Diagnostics      []ChildRecipeIssue        `json:"diagnostics,omitempty"`
}

type RecipeIntegrationBinding struct {
	Status         string `json:"status"`
	ContractID     string `json:"contract_id"`
	ContractDigest string `json:"contract_digest,omitempty"`
	BundleID       string `json:"bundle_id,omitempty"`
	BundleDigest   string `json:"bundle_digest,omitempty"`
}

type RecipeIssueGroup struct {
	Code    string             `json:"code"`
	Count   int                `json:"count"`
	Issues  []ChildRecipeIssue `json:"issues"`
	Recipes []string           `json:"recipes,omitempty"`
}

// LoadIntegrationBundle applies the settings-scoped bundle byte limit without
// requiring every catalog recipe to be executable first.
func LoadIntegrationBundle(settingsPath string, bundlePath string) (*integration.Bundle, error) {
	bundlePath = strings.TrimSpace(bundlePath)
	if bundlePath == "" {
		return nil, nil
	}
	path := resolveSettingsPath(settingsPath)
	settings, err := loadTOML(path)
	if err != nil {
		return nil, fmt.Errorf("load settings %s: %w", path, err)
	}
	limits, err := ParseRuntimeLimits(settings["limits"])
	if err != nil {
		return nil, err
	}
	return integration.LoadBundleFile(expandUser(bundlePath), limits.IntegrationBundleMaxBytes)
}

func BuildRecipeCatalogReportWithOptions(settingsPath string, options RecipeCatalogOptions) (RecipeCatalogReport, error) {
	path := resolveSettingsPath(settingsPath)
	settings, err := loadTOML(path)
	if err != nil {
		return RecipeCatalogReport{}, fmt.Errorf("load settings %s: %w", path, err)
	}
	rawProfileOverrides := asObject(settings["backend_profiles"])
	rawRecipeOverrides := asObject(settings["relay_recipes"])
	transientSources, err := loadTransientRecipeSources(options.TransientSources, rawProfileOverrides, rawRecipeOverrides)
	if err != nil {
		return RecipeCatalogReport{}, err
	}
	rawProfiles := mergeNamedRecords(defaultBackendProfiles, rawProfileOverrides)
	rawRecipes := mergeNamedRecords(defaultRelayRecipes, rawRecipeOverrides)
	normalizedProfiles := NormalizeBackendProfiles(rawProfileOverrides)
	normalizedRecipes := NormalizeRelayRecipes(rawRecipeOverrides)
	populateTransientRecipeDigests(transientSources, normalizedRecipes)
	transientRecipeIDs := map[string]bool{}
	for _, source := range transientSources {
		for _, recipeID := range source.RecipeIDs {
			transientRecipeIDs[recipeID] = true
		}
	}
	transientTraces := transientRecipeDigestTraces(transientSources)
	profileIssues := profileDiagnostics(rawProfiles, normalizedProfiles, rawProfileOverrides)

	type readinessCandidate struct {
		recordIndex int
		backends    []string
	}
	records := make([]RecipeRecord, 0, len(rawRecipes)+len(rawRecipeOverrides))
	readinessCandidates := []readinessCandidate{}
	allBackends := map[string]bool{}
	for _, recipeID := range sortedObjectKeys(rawRecipes) {
		rawRecipe := rawRecipes[recipeID]
		record := RecipeRecord{
			ID:       recipeID,
			Source:   recipeSourceForReport(recipeID, rawRecipeOverrides, transientRecipeIDs),
			Declared: cloneObject(rawRecipe),
		}
		if trace, ok := transientTraces[recipeID]; ok {
			record.SourceDigest = trace.SourceDigest
			record.RecipeDigest = trace.RecipeDigest
		}
		if issue, ok := nonTableRecipeOverrideIssue(recipeID, rawRecipeOverrides); ok {
			record.Status = RecipeStatusSkipped
			record.Diagnostics = []ChildRecipeIssue{issue}
			records = append(records, record)
			continue
		}
		shapeIssues := recipeShapeIssues(recipeID, rawRecipe)
		if len(shapeIssues) > 0 {
			record.Status = RecipeStatusInvalid
			record.Diagnostics = shapeIssues
			records = append(records, record)
			continue
		}
		recipe := normalizedRecipes[recipeID]
		if recipe == nil {
			record.Status = RecipeStatusSkipped
			record.Diagnostics = []ChildRecipeIssue{{
				Category: "invalid_config",
				Code:     "recipe_skipped",
				Message:  fmt.Sprintf("Recipe '%s' was parseable but was skipped by normalization.", recipeID),
				Path:     "relay_recipes." + recipeID,
			}}
			records = append(records, record)
			continue
		}
		participantTurns, turnErr := validatedRootParticipantTurns(recipe)
		if turnErr != nil {
			record.Status = RecipeStatusInvalid
			record.Diagnostics = catalogDiagnosticIssues(turnErr, "invalid_config", "invalid_participant_turns")
			records = append(records, record)
			continue
		}
		issues := rootExecutableIssues(recipe, normalizedProfiles, normalizedRecipes, DepthPolicy{}, "root")
		issues = annotateProfileReferenceIssues(issues, profileIssues)
		record.Diagnostics = issues
		record.Resolved = resolveRecipeView(recipe, normalizedProfiles, normalizedRecipes)
		backends, closureErr := readiness.ResolveBackendClosure(recipe, normalizedProfiles, normalizedRecipes, readiness.ClosureOptions{
			IncludeReducer:        normalizeResultSource(recipe["result_source"]) == integration.ResultSourceReducer,
			IncludeNestedReducers: true,
		})
		if closureErr == nil {
			record.Resolved["backends"] = cleanStringList(backends, false)
		} else if len(issues) == 0 {
			record.Diagnostics = append(record.Diagnostics, ChildRecipeIssue{
				Category: "backend_readiness",
				Code:     "backend_resolution_failed",
				Message:  "Recipe backend closure could not be resolved.",
				Path:     "relay_recipes." + recipeID,
				Detail:   map[string]any{"cause": closureErr.Error()},
			})
		}
		if len(record.Diagnostics) > 0 || closureErr != nil {
			record.Status = RecipeStatusInvalid
			records = append(records, record)
			continue
		}
		record.Integration, issues = catalogIntegrationBinding(recipe, participantTurns, options.IntegrationBundle)
		record.Diagnostics = append(record.Diagnostics, issues...)
		switch {
		case record.Integration != nil && record.Integration.Status != RecipeIntegrationStatusBound:
			record.Status = RecipeStatusRequiresIntegration
		default:
			readinessCandidates = append(readinessCandidates, readinessCandidate{
				recordIndex: len(records),
				backends:    backends,
			})
			for _, backend := range backends {
				allBackends[backend] = true
			}
		}
		records = append(records, record)
	}
	if len(readinessCandidates) > 0 {
		check := options.ReadinessCheck
		if check == nil {
			check = readiness.Check
		}
		ctx := options.Context
		if ctx == nil {
			ctx = context.Background()
		}
		readinessOptions := options.ReadinessOptions
		readinessOptions.ProbeAuth = false
		backendRecords, err := check(ctx, sortedObjectKeys(allBackends), readinessOptions)
		if err != nil {
			return RecipeCatalogReport{}, fmt.Errorf("check recipe backend readiness: %w", err)
		}
		readinessByBackend := map[string]readiness.Record{}
		for _, backendRecord := range backendRecords {
			readinessByBackend[backendRecord.Backend] = backendRecord
		}
		for _, candidate := range readinessCandidates {
			record := &records[candidate.recordIndex]
			for _, backend := range candidate.backends {
				backendRecord, ok := readinessByBackend[backend]
				if !ok {
					record.Diagnostics = append(record.Diagnostics, missingBackendReadinessIssue(backend))
					continue
				}
				record.BackendReadiness = append(record.BackendReadiness, backendRecord)
				if !catalogBackendAvailable(backendRecord) {
					record.Diagnostics = append(record.Diagnostics, backendUnavailableIssue(backendRecord))
				}
			}
			if len(record.Diagnostics) > 0 {
				record.Status = RecipeStatusUnavailable
			} else {
				record.Status = RecipeStatusUsable
			}
		}
	}
	for _, skipped := range skippedRecipeRecords(rawRecipeOverrides, rawRecipes) {
		records = append(records, skipped)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].ID < records[j].ID
	})
	report := RecipeCatalogReport{
		Scope:        "recipes",
		Status:       catalogStatus(records, profileIssues),
		SettingsPath: path,
		Recipes:      records,
		Diagnostics:  flattenProfileDiagnostics(profileIssues),
	}
	report.IssueGroups = BuildRecipeIssueGroups(records, report.Diagnostics)
	return report, nil
}

func FilterRecipeRecords(records []RecipeRecord, status string) []RecipeRecord {
	status = strings.TrimSpace(status)
	if status == "" || status == "all" {
		return append([]RecipeRecord{}, records...)
	}
	filtered := []RecipeRecord{}
	for _, record := range records {
		if record.Status == status {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func FilterRecipeRecordsByStatuses(records []RecipeRecord, statuses ...string) []RecipeRecord {
	wanted := map[string]bool{}
	for _, status := range statuses {
		status = strings.TrimSpace(status)
		if status != "" {
			wanted[status] = true
		}
	}
	filtered := make([]RecipeRecord, 0, len(records))
	for _, record := range records {
		if wanted[record.Status] {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func FindRecipeRecord(records []RecipeRecord, recipeID string) (RecipeRecord, bool) {
	recipeID = strings.TrimSpace(recipeID)
	for _, record := range records {
		if record.ID == recipeID {
			return record, true
		}
	}
	return RecipeRecord{}, false
}

func FormatRecipeList(records []RecipeRecord) string {
	if len(records) == 0 {
		return "No recipes matched."
	}
	lines := []string{"Recipes:"}
	for _, record := range records {
		participants := strings.Join(stringSlice(record.Declared["participants"]), ",")
		if participants == "" {
			participants = "-"
		}
		purpose := strings.TrimSpace(stringValue(record.Declared["purpose"]))
		lines = append(lines, fmt.Sprintf("  %-18s %-20s %-8s participants=%s", record.ID, record.Status, record.Source, participants))
		if purpose != "" {
			lines = append(lines, "    "+purpose)
		}
		if len(record.Diagnostics) > 0 {
			lines = append(lines, fmt.Sprintf("    diagnostics=%d", len(record.Diagnostics)))
		}
	}
	return strings.Join(lines, "\n")
}

func FormatRecipeShow(record RecipeRecord, view string) string {
	view = strings.TrimSpace(view)
	if view == "" {
		view = "all"
	}
	lines := []string{
		fmt.Sprintf("Recipe: %s", record.ID),
		fmt.Sprintf("Status: %s", record.Status),
		fmt.Sprintf("Source: %s", record.Source),
	}
	if record.SourceDigest != "" {
		lines = append(lines, fmt.Sprintf("Source digest: %s", record.SourceDigest))
	}
	if record.RecipeDigest != "" {
		lines = append(lines, fmt.Sprintf("Recipe digest: %s", record.RecipeDigest))
	}
	if record.Integration != nil {
		lines = append(lines, fmt.Sprintf("Integration: %s contract=%s", record.Integration.Status, record.Integration.ContractID))
		if record.Integration.BundleID != "" {
			lines = append(lines, fmt.Sprintf("Integration bundle: %s digest=%s", record.Integration.BundleID, record.Integration.BundleDigest))
		}
	}
	if view == "all" || view == "declared" {
		lines = append(lines, "Declared:")
		lines = append(lines, fmt.Sprintf("  participants: %s", strings.Join(stringSlice(record.Declared["participants"]), ",")))
		lines = append(lines, fmt.Sprintf("  facilitator: %s", stringValue(record.Declared["facilitator"])))
		lines = append(lines, fmt.Sprintf("  reducer: %s", stringValue(record.Declared["reducer"])))
		lines = append(lines, fmt.Sprintf("  mode: %s", stringValue(record.Declared["mode"])))
		lines = append(lines, fmt.Sprintf("  max_rounds: %v", record.Declared["max_rounds"]))
		lines = append(lines, fmt.Sprintf("  max_depth: %v", record.Declared["max_depth"]))
		if purpose := strings.TrimSpace(stringValue(record.Declared["purpose"])); purpose != "" {
			lines = append(lines, "  purpose: "+purpose)
		}
	}
	if (view == "all" || view == "resolved") && len(record.Resolved) > 0 {
		lines = append(lines, "Resolved:")
		if participants, ok := record.Resolved["participants"].([]any); ok {
			for _, raw := range participants {
				participant, _ := raw.(map[string]any)
				lines = append(lines, fmt.Sprintf("  %s: profile=%s backend=%s model=%v effort=%v",
					stringValue(participant["slot_id"]),
					stringValue(participant["profile_id"]),
					stringValue(participant["backend"]),
					participant["model"],
					participant["effort"],
				))
			}
		}
		for _, role := range []string{"facilitator", "reducer"} {
			if resolved, ok := record.Resolved[role].(map[string]any); ok {
				lines = append(lines, fmt.Sprintf("  %s: profile=%s backend=%s model=%v effort=%v",
					role,
					stringValue(resolved["profile_id"]),
					stringValue(resolved["backend"]),
					resolved["model"],
					resolved["effort"],
				))
			}
		}
		if len(record.BackendReadiness) > 0 {
			lines = append(lines, "  backend readiness:")
			for _, backend := range record.BackendReadiness {
				lines = append(lines, fmt.Sprintf("    %s: %s", backend.Backend, backend.Status))
			}
		}
	}
	if len(record.Diagnostics) > 0 {
		lines = append(lines, "Diagnostics:")
		for _, issue := range record.Diagnostics {
			lines = append(lines, fmt.Sprintf("  - %s: %s", issue.Code, issue.Message))
		}
	}
	return strings.Join(lines, "\n")
}

func FormatRecipeDoctor(report RecipeCatalogReport) string {
	lines := []string{
		fmt.Sprintf("Recipe doctor: %s", report.Status),
		fmt.Sprintf("Settings: %s", report.SettingsPath),
	}
	for _, group := range report.IssueGroups {
		lines = append(lines, fmt.Sprintf("- %s: %d", group.Code, group.Count))
		for _, issue := range group.Issues {
			lines = append(lines, fmt.Sprintf("  %s: %s", issue.Path, issue.Message))
		}
	}
	if len(report.IssueGroups) == 0 {
		lines = append(lines, "No recipe issues found.")
	}
	return strings.Join(lines, "\n")
}

func BuildRecipeIssueGroups(records []RecipeRecord, globalIssues []ChildRecipeIssue) []RecipeIssueGroup {
	grouped := map[string]*RecipeIssueGroup{}
	addIssue := func(recipeID string, issue ChildRecipeIssue) {
		code := strings.TrimSpace(issue.Code)
		if code == "" {
			code = "unknown"
		}
		group := grouped[code]
		if group == nil {
			group = &RecipeIssueGroup{Code: code}
			grouped[code] = group
		}
		group.Count++
		group.Issues = append(group.Issues, issue)
		if recipeID != "" && !containsString(group.Recipes, recipeID) {
			group.Recipes = append(group.Recipes, recipeID)
		}
	}
	for _, issue := range globalIssues {
		addIssue("", issue)
	}
	for _, record := range records {
		for _, issue := range record.Diagnostics {
			addIssue(record.ID, issue)
		}
	}
	keys := sortedObjectKeys(grouped)
	groups := make([]RecipeIssueGroup, 0, len(keys))
	for _, key := range keys {
		group := *grouped[key]
		sort.Strings(group.Recipes)
		groups = append(groups, group)
	}
	return groups
}

func recipeShapeIssues(recipeID string, recipe map[string]any) []ChildRecipeIssue {
	diagnostics := validateRecipeRecord(recipe, appendRecipePointer("/relay_recipes", recipeID))
	issues := make([]ChildRecipeIssue, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		issues = append(issues, ChildRecipeIssue{
			Category: "invalid_config",
			Code:     diagnostic.Code,
			Message:  diagnostic.Message,
			Path:     diagnostic.Path,
			Detail:   diagnostic.Details,
		})
	}
	return issues
}

func profileDiagnostics(rawProfiles map[string]map[string]any, normalized map[string]map[string]any, overrides map[string]any) map[string][]ChildRecipeIssue {
	issues := map[string][]ChildRecipeIssue{}
	for _, profileID := range sortedObjectKeys(rawProfiles) {
		profile := rawProfiles[profileID]
		backend := strings.TrimSpace(stringValue(profile["backend"]))
		if backend == "" || !backendRegistry[backend] {
			issues[profileID] = append(issues[profileID], ChildRecipeIssue{
				Category: "invalid_config",
				Code:     "unsupported_backend",
				Message:  fmt.Sprintf("Backend profile '%s' uses unsupported backend '%s'.", profileID, backend),
				Path:     "backend_profiles." + profileID + ".backend",
				Detail:   map[string]any{"profile_id": profileID, "backend": backend},
			})
			continue
		}
		if normalized[profileID] == nil {
			issues[profileID] = append(issues[profileID], ChildRecipeIssue{
				Category: "invalid_config",
				Code:     "profile_skipped",
				Message:  fmt.Sprintf("Backend profile '%s' was parseable but was skipped by normalization.", profileID),
				Path:     "backend_profiles." + profileID,
				Detail:   map[string]any{"profile_id": profileID},
			})
		}
	}
	for _, profileID := range sortedObjectKeys(overrides) {
		if _, ok := overrides[profileID].(map[string]any); !ok {
			issues[profileID] = append(issues[profileID], ChildRecipeIssue{
				Category: "invalid_config",
				Code:     "profile_record_not_table",
				Message:  fmt.Sprintf("Backend profile '%s' must be a TOML table.", profileID),
				Path:     "backend_profiles." + profileID,
				Detail:   map[string]any{"profile_id": profileID},
			})
		}
	}
	return issues
}

func annotateProfileReferenceIssues(issues []ChildRecipeIssue, profileIssues map[string][]ChildRecipeIssue) []ChildRecipeIssue {
	result := make([]ChildRecipeIssue, 0, len(issues))
	for _, issue := range issues {
		ref := stringValue(issue.Detail["profile_ref"])
		if ref != "" && len(profileIssues[ref]) > 0 && strings.HasPrefix(issue.Code, "unknown_") {
			issue.Code = "invalid_profile_reference"
			issue.Message = fmt.Sprintf("Profile '%s' is invalid; fix the backend profile root cause first.", ref)
			issue.Detail["profile_ref"] = ref
		}
		result = append(result, issue)
	}
	return result
}

func resolveRecipeView(recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any) map[string]any {
	resolved := map[string]any{}
	participants := []any{}
	for index, ref := range stringSlice(recipe["participants"]) {
		if profile, err := compiledProfile(ref, profiles, relayRecipes, fmt.Sprintf("slot_%d", index), fmt.Sprintf("root.slot_%d", index)); err == nil {
			participants = append(participants, profile)
		}
	}
	resolved["participants"] = participants
	if profile, err := compiledProfile(stringValue(recipe["facilitator"]), profiles, relayRecipes, "facilitator", "root.facilitator"); err == nil {
		resolved["facilitator"] = profile
	}
	if profile, err := compiledProfile(stringValue(recipe["reducer"]), profiles, relayRecipes, "reducer", "root.reducer"); err == nil {
		resolved["reducer"] = profile
	}
	resolved["backends"] = resolvedBackends(participants)
	return resolved
}

func resolvedBackends(participants []any) []any {
	backends := []any{}
	for _, raw := range participants {
		participant, _ := raw.(map[string]any)
		backend := strings.TrimSpace(stringValue(participant["backend"]))
		if backend != "" && !containsAnyString(backends, backend) {
			backends = append(backends, backend)
		}
	}
	return backends
}

func skippedRecipeRecords(overrides map[string]any, merged map[string]map[string]any) []RecipeRecord {
	records := []RecipeRecord{}
	for _, recipeID := range sortedObjectKeys(overrides) {
		if _, ok := overrides[recipeID].(map[string]any); ok {
			continue
		}
		if _, exists := merged[recipeID]; exists {
			continue
		}
		records = append(records, RecipeRecord{
			ID:          recipeID,
			Status:      RecipeStatusSkipped,
			Source:      "settings",
			Diagnostics: []ChildRecipeIssue{recipeRecordNotTableIssue(recipeID)},
		})
	}
	return records
}

func nonTableRecipeOverrideIssue(recipeID string, overrides map[string]any) (ChildRecipeIssue, bool) {
	raw, ok := overrides[recipeID]
	if !ok {
		return ChildRecipeIssue{}, false
	}
	if _, ok := raw.(map[string]any); ok {
		return ChildRecipeIssue{}, false
	}
	return recipeRecordNotTableIssue(recipeID), true
}

func recipeRecordNotTableIssue(recipeID string) ChildRecipeIssue {
	return ChildRecipeIssue{
		Category: "invalid_config",
		Code:     "recipe_record_not_table",
		Message:  fmt.Sprintf("Recipe '%s' must be a TOML table.", recipeID),
		Path:     "relay_recipes." + recipeID,
		Detail:   map[string]any{"recipe_id": recipeID},
	}
}

func flattenProfileDiagnostics(profileIssues map[string][]ChildRecipeIssue) []ChildRecipeIssue {
	issues := []ChildRecipeIssue{}
	for _, profileID := range sortedObjectKeys(profileIssues) {
		issues = append(issues, profileIssues[profileID]...)
	}
	return issues
}

func catalogIntegrationBinding(recipe map[string]any, participantTurns int, bundle *integration.Bundle) (*RecipeIntegrationBinding, []ChildRecipeIssue) {
	contractID := stringValue(recipe["integration_contract"])
	if strings.TrimSpace(contractID) == "" {
		return nil, nil
	}
	binding := &RecipeIntegrationBinding{
		Status:     RecipeIntegrationStatusRequired,
		ContractID: contractID,
	}
	if bundle == nil {
		return binding, []ChildRecipeIssue{{
			Category: "integration_binding",
			Code:     "integration_required",
			Message:  fmt.Sprintf("Recipe '%s' requires integration contract '%s'.", stringValue(recipe["id"]), contractID),
			Path:     "recipe.integration_contract",
			Detail:   map[string]any{"contract_id": contractID},
		}}
	}
	binding.BundleID = bundle.ID()
	binding.BundleDigest = bundle.Digest()
	scheduledTurns, err := integration.AlternatingSchedule(participantTurns)
	if err != nil {
		return binding, []ChildRecipeIssue{{
			Category: "integration_binding",
			Code:     "integration_schedule_invalid",
			Message:  "Recipe participant schedule cannot be matched to an integration contract.",
			Path:     "recipe.participant_turns",
			Detail:   map[string]any{"cause": err.Error()},
		}}
	}
	selected, err := integration.SelectContract(bundle, contractID, integration.ScheduleRequirement{
		Turns:        scheduledTurns,
		ResultSource: normalizeResultSource(recipe["result_source"]),
	})
	if err != nil {
		return binding, catalogIntegrationIssues(err)
	}
	binding.Status = RecipeIntegrationStatusBound
	binding.ContractDigest = selected.Digest()
	return binding, nil
}

func catalogIntegrationIssues(err error) []ChildRecipeIssue {
	return catalogDiagnosticIssues(err, "integration_binding", "integration_binding_failed")
}

func catalogDiagnosticIssues(err error, category string, fallbackCode string) []ChildRecipeIssue {
	var diagnosticError *format.DiagnosticError
	if errors.As(err, &diagnosticError) && len(diagnosticError.Diagnostics) > 0 {
		issues := make([]ChildRecipeIssue, 0, len(diagnosticError.Diagnostics))
		for _, diagnostic := range diagnosticError.Diagnostics {
			issues = append(issues, ChildRecipeIssue{
				Category: category,
				Code:     diagnostic.Code,
				Message:  diagnostic.Message,
				Path:     diagnostic.Path,
				Detail:   diagnostic.Details,
			})
		}
		return issues
	}
	return []ChildRecipeIssue{{
		Category: category,
		Code:     fallbackCode,
		Message:  err.Error(),
		Detail:   map[string]any{"cause": err.Error()},
	}}
}

func catalogBackendAvailable(record readiness.Record) bool {
	return record.Status == readiness.StatusInstalledAuthUnknown || record.Status == readiness.StatusReady
}

func backendUnavailableIssue(record readiness.Record) ChildRecipeIssue {
	detail := map[string]any{
		"backend":               record.Backend,
		"status":                record.Status,
		"authentication_status": record.AuthenticationStatus,
	}
	if record.ExecutablePath != "" {
		detail["executable_path"] = record.ExecutablePath
	}
	if record.Version != "" {
		detail["version"] = record.Version
	}
	return ChildRecipeIssue{
		Category: "backend_readiness",
		Code:     "backend_unavailable",
		Message:  fmt.Sprintf("Backend '%s' is unavailable under the installation readiness policy (%s).", record.Backend, record.Status),
		Path:     "backend:" + record.Backend,
		Detail:   detail,
	}
}

func missingBackendReadinessIssue(backend string) ChildRecipeIssue {
	return ChildRecipeIssue{
		Category: "backend_readiness",
		Code:     "backend_readiness_missing",
		Message:  fmt.Sprintf("Backend '%s' has no readiness result.", backend),
		Path:     "backend:" + backend,
		Detail:   map[string]any{"backend": backend},
	}
}

func catalogStatus(records []RecipeRecord, profileIssues map[string][]ChildRecipeIssue) string {
	if len(profileIssues) > 0 {
		return "degraded"
	}
	for _, record := range records {
		if record.Status != RecipeStatusUsable && record.Status != RecipeStatusRequiresIntegration {
			return "degraded"
		}
	}
	return "ok"
}

func recipeSource(recipeID string, overrides map[string]any) string {
	if _, ok := overrides[recipeID]; ok {
		if _, builtin := defaultRelayRecipes[recipeID]; builtin {
			return "settings+builtin"
		}
		return "settings"
	}
	return "builtin"
}

func recipeSourceForReport(recipeID string, overrides map[string]any, transientRecipeIDs map[string]bool) string {
	if transientRecipeIDs[recipeID] {
		if _, builtin := defaultRelayRecipes[recipeID]; builtin {
			return "transient+builtin"
		}
		return "transient"
	}
	return recipeSource(recipeID, overrides)
}

func containsAnyString(values []any, candidate string) bool {
	for _, value := range values {
		if stringValue(value) == candidate {
			return true
		}
	}
	return false
}
