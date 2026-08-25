// Package relayv2 contains the concrete CLI-facing adapters for the v2
// plan/session/engine stack. It is intentionally small: execution authority
// remains in engine, while this package supplies provider construction and
// the durable local runtime data needed by later CLI invocations.
package relayv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/engine"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/provider"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const runtimeFilename = "v2-runtime.json"

// Runtime is local execution configuration, deliberately outside the
// portable immutable plan. It records no output and is only used to recreate
// the imperative provider boundary for resume and operator child decisions.
type Runtime struct {
	SchemaVersion int           `json:"schema_version"`
	SettingsPath  string        `json:"settings_path,omitempty"`
	Recipes       []plan.Recipe `json:"recipes"`
}

// SaveRuntime records local v2 execution dependencies once a session has
// been created. The runtime directory is owned by session.Create.
func SaveRuntime(sess *session.Session, value Runtime) error {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return errors.New("session is required to save v2 runtime")
	}
	value.SchemaVersion = 1
	value.Recipes = append([]plan.Recipe{}, value.Recipes...)
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode v2 runtime: %w", err)
	}
	filename := filepath.Join(sess.Root, "runtime", runtimeFilename)
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return fmt.Errorf("create v2 runtime directory: %w", err)
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create v2 runtime: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write v2 runtime: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync v2 runtime: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close v2 runtime: %w", err)
	}
	return nil
}

// LoadRuntime recovers the local dependency snapshot for a v2 session.
func LoadRuntime(sess *session.Session) (Runtime, error) {
	if sess == nil || strings.TrimSpace(sess.Root) == "" {
		return Runtime{}, errors.New("session is required to load v2 runtime")
	}
	body, err := os.ReadFile(filepath.Join(sess.Root, "runtime", runtimeFilename))
	if err != nil {
		return Runtime{}, fmt.Errorf("read v2 runtime: %w", err)
	}
	var value Runtime
	if err := json.Unmarshal(body, &value); err != nil {
		return Runtime{}, fmt.Errorf("decode v2 runtime: %w", err)
	}
	if value.SchemaVersion != 1 {
		return Runtime{}, fmt.Errorf("unsupported v2 runtime schema version %d", value.SchemaVersion)
	}
	for index, recipe := range value.Recipes {
		if strings.TrimSpace(recipe.ID) == "" {
			return Runtime{}, fmt.Errorf("v2 runtime recipe %d has no id", index)
		}
	}
	return value, nil
}

// NewDeps builds the complete engine dependency boundary for a local CLI
// execution. A child session shares the same launch workspace and catalog as
// its admitting parent, while still receiving its own session root.
func NewDeps(value Runtime, executionCWD string) engine.Deps {
	return engine.Deps{
		BackendFactory:        NewBackendFactory(value, executionCWD),
		Recipes:               append([]plan.Recipe{}, value.Recipes...),
		ChildRequestExtractor: NewChildRequestExtractor(value.Recipes),
	}
}

// ExecutionCWD recovers the durable workspace boundary for a later operation.
func ExecutionCWD(ctx context.Context, sess *session.Session) (string, error) {
	if sess == nil {
		return "", errors.New("session is required")
	}
	recovered, err := workspace.Recover(ctx, sess)
	if err != nil {
		return "", err
	}
	return recovered.ExecutionCWD, nil
}

// NewBackendFactory is the production engine.BackendFactory. It maps the
// immutable session and actor to provider.NewBackend's six arguments without
// reconstructing provider policy at the call site.
func NewBackendFactory(value Runtime, executionCWD string) engine.BackendFactory {
	return func(sess *session.Session, actor session.Actor) (provider.Backend, error) {
		if sess == nil || strings.TrimSpace(sess.Root) == "" {
			return nil, errors.New("backend session is required")
		}
		backendName := strings.TrimSpace(actor.Backend)
		if !provider.KnownBackend(backendName) {
			return nil, fmt.Errorf("unknown provider backend %q", backendName)
		}
		if isFacilitator(sess.Plan, actor.ID) && !provider.FacilitatorBackendAllowed(backendName) {
			return nil, fmt.Errorf("provider backend %q is not allowed for facilitator", backendName)
		}
		cwd := strings.TrimSpace(executionCWD)
		if cwd == "" {
			return nil, errors.New("provider execution cwd is required")
		}
		return provider.NewBackend(
			backendName,
			sess.Root,
			actor.ID,
			provider.BackendLabel(backendName),
			cwd,
			provider.SlotConfig{
				ProfileID:    actor.ProfileID,
				Model:        actor.Model,
				Effort:       actor.Effort,
				SettingsPath: value.SettingsPath,
			},
		)
	}
}

func isFacilitator(value session.Plan, actorID string) bool {
	return value.Facilitator != nil && value.Facilitator.Actor == actorID
}

// NewChildRequestExtractor turns a facilitator ledger into durable child
// requests. Its only dependency on provider.TurnResult is Content: recovery
// reconstructs precisely that field before invoking extraction again.
func NewChildRequestExtractor(catalog []plan.Recipe) engine.ChildRequestExtractor {
	recipes := append([]plan.Recipe{}, catalog...)
	sort.Slice(recipes, func(left, right int) bool { return recipes[left].ID < recipes[right].ID })
	return func(parent session.Plan, _ session.Actor, role eventlog.Role, result provider.TurnResult) []engine.ChildRequest {
		if role != eventlog.FacilitatorRole {
			return nil
		}
		contested, ok := contestedLedgerItems(result.Content)
		if !ok || len(contested) == 0 {
			return nil
		}
		requests := make([]engine.ChildRequest, 0, len(contested))
		for _, item := range contested {
			recipeID := childRecipeForItem(parent.ChildPolicy, recipes)
			if recipeID == "" {
				continue
			}
			requests = append(requests, engine.ChildRequest{
				ID: childRequestID(item),
				Request: plan.ChildRequest{
					RecipeID: recipeID,
					Question: "Resolve this contested parent-relay item: " + item,
				},
			})
		}
		return requests
	}
}

func contestedLedgerItems(content string) ([]string, bool) {
	var document map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	if err := decoder.Decode(&document); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	settled, ok := ledgerStrings(document["settled"])
	if !ok {
		return nil, false
	}
	contested, ok := ledgerStrings(document["contested"])
	if !ok {
		return nil, false
	}
	withdrawn, ok := ledgerStrings(document["withdrawn"])
	if !ok {
		return nil, false
	}
	// Parse all three arrays even though only contested drives requests. A
	// partial JSON object is not a ledger document and must not accidentally
	// become a child-spawn protocol.
	_ = settled
	_ = withdrawn
	items := make([]string, 0, len(contested))
	seen := map[string]bool{}
	for _, raw := range contested {
		item := strings.TrimSpace(raw)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		items = append(items, item)
	}
	return items, true
}

func ledgerStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, false
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false
	}
	return values, true
}

// childRecipeForItem selects the first allowed recipe declared by the parent
// child policy that is present in the catalog; otherwise it selects the
// review-panel convention; otherwise it selects the first catalog recipe by
// sorted ID.
func childRecipeForItem(policy session.ChildPolicy, catalog []plan.Recipe) string {
	byID := make(map[string]bool, len(catalog))
	for _, recipe := range catalog {
		byID[recipe.ID] = true
	}
	for _, allowed := range policy.AllowedRecipes {
		if recipeID := strings.TrimSpace(allowed); recipeID != "" && byID[recipeID] {
			return recipeID
		}
	}
	for _, recipe := range catalog {
		if recipe.ID == "review-panel" {
			return recipe.ID
		}
	}
	if len(catalog) != 0 {
		return catalog[0].ID
	}
	return ""
}

func childRequestID(item string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(item)), " ")
	sum := sha256.Sum256([]byte(normalized))
	return "child-" + hex.EncodeToString(sum[:])[:16]
}

// RecipeFromRuntime converts a normalized legacy configuration record into
// the typed recipe accepted by plan.FromRecipe. Profile resolution happens at
// this edge, before the plan compiler, so engine never consumes raw config.
func RecipeFromRuntime(config recipes.RuntimeConfig, recipeID string) (plan.Recipe, error) {
	record, found := config.RelayRecipes[strings.TrimSpace(recipeID)]
	if !found || record == nil {
		return plan.Recipe{}, fmt.Errorf("recipe %q was not found", recipeID)
	}
	return recipeFromRecord(record, config.BackendProfiles)
}

// RecipesFromRuntime returns the executable provider-backed subset of a
// catalog. Unsupported relay pseudo-backends are deliberately omitted: they
// are not constructible by provider.NewBackend and cannot silently enter v2.
func RecipesFromRuntime(config recipes.RuntimeConfig) ([]plan.Recipe, error) {
	ids := make([]string, 0, len(config.RelayRecipes))
	for recipeID := range config.RelayRecipes {
		ids = append(ids, recipeID)
	}
	sort.Strings(ids)
	items := make([]plan.Recipe, 0, len(ids))
	for _, recipeID := range ids {
		recipe, err := RecipeFromRuntime(config, recipeID)
		if err != nil {
			continue
		}
		items = append(items, recipe)
	}
	return items, nil
}

func recipeFromRecord(record map[string]any, profiles map[string]map[string]any) (plan.Recipe, error) {
	normalized := recipes.RecipeContractPayload(record)
	id := strings.TrimSpace(stringValue(normalized["id"]))
	if id == "" {
		return plan.Recipe{}, errors.New("recipe id is required")
	}
	participantRefs := stringValues(normalized["participants"])
	if len(participantRefs) != 2 {
		return plan.Recipe{}, fmt.Errorf("recipe %q must contain two participants", id)
	}
	actors := make([]session.Actor, 0, 4)
	for index, reference := range participantRefs {
		actor, err := actorFromProfile(fmt.Sprintf("slot_%d", index), reference, profiles)
		if err != nil {
			return plan.Recipe{}, fmt.Errorf("recipe %q participant %d: %w", id, index, err)
		}
		actors = append(actors, actor)
	}
	facilitator, err := actorFromProfile("facilitator", stringValue(normalized["facilitator"]), profiles)
	if err != nil {
		return plan.Recipe{}, fmt.Errorf("recipe %q facilitator: %w", id, err)
	}
	actors = append(actors, facilitator)
	resultSource := stringValue(normalized["result_source"])
	var reducer *session.Reducer
	if resultSource == integration.ResultSourceReducer {
		reducerActor, err := actorFromProfile("reducer", stringValue(normalized["reducer"]), profiles)
		if err != nil {
			return plan.Recipe{}, fmt.Errorf("recipe %q reducer: %w", id, err)
		}
		actors = append(actors, reducerActor)
		reducer = &session.Reducer{Actor: reducerActor.ID}
	}
	lifecycleRecord, _ := normalized["lifecycle"].(map[string]any)
	lifecycle := session.Lifecycle{
		Resume:   stringValue(lifecycleRecord["resume"]),
		Steering: stringValue(lifecycleRecord["steering"]),
		Dynamic:  stringValue(lifecycleRecord["dynamic"]),
	}
	workspacePlan := session.Workspace{Mode: workspace.ModeCurrent}
	if stringValue(lifecycleRecord["workspace_isolation"]) == "ephemeral" {
		workspacePlan.Mode = workspace.ModeHeadCopy
	}
	retryMode := recipes.EffectiveProviderRetry(normalized)
	retry := session.ProviderRetry{Mode: retryMode, MaxAttempts: 7}
	if retryMode == recipes.ProviderRetryForbid {
		retry.MaxAttempts = 1
	}
	return plan.Recipe{
		Kind:          "recipe",
		SchemaVersion: intValue(normalized["schema_version"], 1),
		ID:            id,
		Purpose:       stringValue(normalized["purpose"]),
		Actors:        actors,
		Schedule: session.Schedule{
			Kind:  "dialogue",
			Turns: intValue(normalized["participant_turns"], intValue(normalized["max_rounds"], 1)),
		},
		Mode:                stringValue(normalized["mode"]),
		Facilitator:         &session.Facilitator{Actor: facilitator.ID, Cadence: 1},
		Reducer:             reducer,
		MaxRounds:           intValue(normalized["max_rounds"], 1),
		ParticipantTurns:    intValue(normalized["participant_turns"], intValue(normalized["max_rounds"], 1)),
		ResultSource:        resultSource,
		ProviderRetry:       retry,
		IntegrationContract: stringValue(normalized["integration_contract"]),
		MaxDepth:            intValue(normalized["max_depth"], 1),
		AutoApproval:        stringValue(normalized["auto_approval"]),
		Lifecycle:           lifecycle,
		Workspace:           workspacePlan,
		ChildPolicy: session.ChildPolicy{
			Mode:           childPolicyMode(stringValue(normalized["auto_approval"])),
			MaxDepth:       intValue(normalized["max_depth"], 1),
			MaxChildren:    2,
			MaxTurns:       8,
			AllowedRecipes: []string{},
		},
		Result: session.Result{Source: resultSource, Format: "text"},
	}, nil
}

func actorFromProfile(id, reference string, profiles map[string]map[string]any) (session.Actor, error) {
	profile, err := recipes.ResolveProfileRef(reference, profiles)
	if err != nil {
		return session.Actor{}, err
	}
	backend := strings.TrimSpace(stringValue(profile["backend"]))
	if !provider.KnownBackend(backend) {
		return session.Actor{}, fmt.Errorf("profile %q resolves to unsupported v2 backend %q", reference, backend)
	}
	return session.Actor{
		ID:        id,
		Backend:   backend,
		Model:     stringValue(profile["model"]),
		Effort:    stringValue(profile["effort"]),
		ProfileID: firstNonEmpty(stringValue(profile["id"]), reference),
	}, nil
}

func childPolicyMode(value string) string {
	switch strings.TrimSpace(value) {
	case "ask":
		return "ask"
	case "auto-safe":
		return "allow"
	default:
		return "deny"
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func stringValues(value any) []string {
	items := []string{}
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
	case []any:
		for _, raw := range typed {
			if item := stringValue(raw); item != "" {
				items = append(items, item)
			}
		}
	}
	return items
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
