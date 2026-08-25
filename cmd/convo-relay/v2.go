package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/engine"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/namedinputs"
	"github.com/charlesnpx/convo-relay/internal/plan"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/relayv2"
	"github.com/charlesnpx/convo-relay/internal/session"
	"github.com/charlesnpx/convo-relay/internal/sessionview"
	"github.com/charlesnpx/convo-relay/internal/workspace"
)

const (
	v2PromptInputMaxBytes   = int64(1 << 20)
	v2PromptInputTotalBytes = int64(2 << 20)
	v2TextMediaType         = "text/plain; charset=utf-8"
)

type v2Input struct {
	Input session.Input
	Body  []byte
}

type v2WorkspaceOptions struct {
	LaunchCWD string
	Mode      string
}

type v2WorkspaceWarning struct {
	Message          string
	StagedChanges    int64
	UnstagedChanges  int64
	UntrackedChanges int64
}

type v2OrdinaryRunOptions struct {
	SessionDir          string
	SessionID           string
	RelayHome           string
	Task                string
	Agents              string
	Rounds              int
	MaxRounds           int
	TimeoutSeconds      int
	StallTimeoutSeconds int
	Mode                string
	Dynamic             string
	Investigation       string
	SettingsPath        string
	LaunchCWD           string
	FacilitatorBackend  string
	FacilitatorModel    string
	FacilitatorEffort   string
	ModelA              string
	EffortA             string
	ModelB              string
	EffortB             string
	Quick               bool
	ContextFiles        []string
	SkillFiles          []string
	TransientSources    []recipes.TransientRecipeSource
	LaunchPlan          any
}

type v2RecipeRunOptions struct {
	SessionDir            string
	SessionID             string
	RelayHome             string
	Task                  string
	RecipeID              string
	ContextFiles          []string
	SkillFiles            []string
	TransientSources      []recipes.TransientRecipeSource
	IntegrationBundlePath string
	InputBindings         []string
	WorkspaceMode         string
	AllowDirtySource      bool
	SettingsPath          string
	LaunchCWD             string
	TimeoutSeconds        int
	StallTimeoutSeconds   int
	Investigation         string
	LaunchPlan            any
	WorkspaceWarning      func(v2WorkspaceWarning)
}

type v2ResumeOptions struct {
	Prompt         string
	RequestedTurns int
}

// v2ProposalReport derives proposal state from the graph's one event projection.
func v2ProposalReport(sess *session.Session) (map[string]any, error) {
	graphReport, err := relayv2.BuildGraphReport(sess)
	if err != nil {
		return nil, err
	}
	graphData, _ := graphReport["graph"].(map[string]any)
	projections, _ := graphData["proposals"].(map[string]any)
	items := make([]any, 0, len(projections))
	for _, requestID := range relayv2.SortProposalIDs(projections) {
		items = append(items, projections[requestID])
	}
	return map[string]any{"session_id": sess.Plan.SessionID, "proposals": items}, nil
}

// v2CancelReport deliberately does not append an event. A separate control
// process cannot append while the executing engine owns events.lock, and the
// v2 format intentionally has no persisted PID to signal. The writer lease is
// therefore only an authoritative liveness check; an active run must receive
// its interrupt directly.
func v2CancelReport(sess *session.Session) (map[string]any, error) {
	lease, err := eventlog.AcquireWriterLease(sess.Root)
	if err != nil {
		var locked *eventlog.WriterLockedError
		if errors.As(err, &locked) {
			return nil, fmt.Errorf("session %s is running; interrupt its relay process directly", sess.Plan.SessionID)
		}
		return nil, err
	}
	defer func() {
		_ = lease.Release()
	}()
	events, err := relayv2.Events(sess)
	if err != nil {
		return nil, err
	}
	status := sessionview.Status(sess.Plan, events)
	if !status.Terminal {
		return nil, errors.New("no owning process is active; the session is not running")
	}
	return map[string]any{"session_id": sess.Plan.SessionID, "status": sessionview.PublicStatus(status.Status, status.StopReason)}, nil
}

func v2RunOrdinary(ctx context.Context, options v2OrdinaryRunOptions) (map[string]any, error) {
	contexts, err := v2ReadPromptInputs(options.ContextFiles, "ctx", "")
	if err != nil {
		return nil, err
	}
	skills, err := v2ReadPromptInputs(options.SkillFiles, "skill", "")
	if err != nil {
		return nil, err
	}
	taskPlan, err := v2TaskPlan(options.LaunchPlan)
	if err != nil {
		return nil, err
	}
	config, _, err := recipes.LoadRuntimeConfigWithTransientSources(options.SettingsPath, options.TransientSources)
	if err != nil {
		return nil, err
	}
	catalog, err := relayv2.RecipesFromRuntime(config)
	if err != nil {
		return nil, err
	}
	planValue, err := plan.FromFlags(plan.Flags{
		SessionID:           v2SessionID(options.SessionID),
		Task:                options.Task,
		Mode:                options.Mode,
		Investigation:       options.Investigation,
		Agents:              options.Agents,
		Rounds:              options.Rounds,
		MaxRounds:           options.MaxRounds,
		Quick:               options.Quick,
		ModelA:              options.ModelA,
		EffortA:             options.EffortA,
		ModelB:              options.ModelB,
		EffortB:             options.EffortB,
		FacilitatorBackend:  options.FacilitatorBackend,
		FacilitatorModel:    options.FacilitatorModel,
		FacilitatorEffort:   options.FacilitatorEffort,
		TimeoutSeconds:      options.TimeoutSeconds,
		StallTimeoutSeconds: options.StallTimeoutSeconds,
		Dynamic:             options.Dynamic,
		Workspace:           session.Workspace{Mode: workspace.ModeCurrent},
		Context:             v2Inputs(contexts),
		Skills:              v2Inputs(skills),
		TaskPlan:            taskPlan,
		ChildPolicy:         session.ChildPolicy{},
		Result:              session.Result{},
	})
	if err != nil {
		return nil, err
	}
	sess, err := v2CreateSession(options.RelayHome, options.SessionDir, planValue)
	if err != nil {
		return nil, err
	}
	launchCWD := v2LaunchCWD(options.LaunchCWD)
	runtime := relayv2.Runtime{SettingsPath: config.SettingsPath, Recipes: catalog}
	if err := v2PersistInputs(sess, contexts, skills); err != nil {
		return nil, err
	}
	if err := relayv2.SaveRuntime(sess, runtime); err != nil {
		return nil, err
	}
	materialized, err := v2PrepareWorkspace(ctx, sess, v2WorkspaceOptions{
		LaunchCWD: launchCWD,
		Mode:      planValue.Workspace.Mode,
	})
	if err != nil {
		return nil, err
	}
	return v2Run(ctx, sess, runtime, materialized.ExecutionCWD, false, "", 0)
}

func v2RunRecipe(ctx context.Context, options v2RecipeRunOptions) (map[string]any, error) {
	workspaceMode, err := v2WorkspaceMode(options.WorkspaceMode)
	if err != nil {
		return nil, err
	}
	config, _, err := recipes.LoadRuntimeConfigWithTransientSources(options.SettingsPath, options.TransientSources)
	if err != nil {
		return nil, err
	}
	contexts, err := v2ReadPromptInputs(options.ContextFiles, "ctx", options.LaunchCWD)
	if err != nil {
		return nil, err
	}
	skills, err := v2ReadPromptInputs(options.SkillFiles, "skill", options.LaunchCWD)
	if err != nil {
		return nil, err
	}
	inputs, err := v2ReadNamedInputs(options.InputBindings, options.LaunchCWD, config.EffectiveLimits())
	if err != nil {
		return nil, err
	}
	taskPlan, err := v2TaskPlan(options.LaunchPlan)
	if err != nil {
		return nil, err
	}
	bundle, err := recipes.LoadIntegrationBundle(config.SettingsPath, options.IntegrationBundlePath)
	if err != nil {
		return nil, err
	}
	planValue, catalog, err := v2RecipePlan(
		config,
		options.RecipeID,
		v2SessionID(options.SessionID),
		options.Task,
		session.Timeouts{TurnSeconds: options.TimeoutSeconds, StallSeconds: options.StallTimeoutSeconds},
		options.Investigation,
		contexts,
		skills,
		namedinputs.Inputs(inputs),
		taskPlan,
		bundle,
	)
	if err != nil {
		return nil, err
	}
	sess, err := v2CreateSession(options.RelayHome, options.SessionDir, planValue)
	if err != nil {
		return nil, err
	}
	launchCWD := v2LaunchCWD(options.LaunchCWD)
	runtime := relayv2.Runtime{SettingsPath: config.SettingsPath, Recipes: catalog}
	if err := v2PersistInputs(sess, contexts, skills); err != nil {
		return nil, err
	}
	if err := namedinputs.Persist(sess, inputs); err != nil {
		return nil, err
	}
	if err := relayv2.SaveRuntime(sess, runtime); err != nil {
		return nil, err
	}
	materialized, err := v2PrepareWorkspace(ctx, sess, v2WorkspaceOptions{LaunchCWD: launchCWD, Mode: workspaceMode})
	if err != nil {
		return nil, err
	}
	return v2Run(ctx, sess, runtime, materialized.ExecutionCWD, false, "", 0)
}

func v2RunResume(ctx context.Context, sessionDir string, options v2ResumeOptions) (map[string]any, error) {
	sess, err := session.Open(sessionDir)
	if err != nil {
		return nil, err
	}
	if _, err := plan.ForResume(sess.Plan, plan.ResumeInput{Prompt: options.Prompt}); err != nil {
		return nil, err
	}
	runtime, err := relayv2.LoadRuntime(sess)
	if err != nil {
		return nil, err
	}
	executionCWD, err := relayv2.ExecutionCWD(ctx, sess)
	if err != nil {
		return nil, err
	}
	return v2Run(ctx, sess, runtime, executionCWD, true, options.Prompt, options.RequestedTurns)
}

func v2WorkspaceMode(mode string) (string, error) {
	if mode != workspace.ModeCurrent && mode != workspace.ModeHeadCopy {
		return "", fmt.Errorf("workspace mode must be %s or %s", workspace.ModeCurrent, workspace.ModeHeadCopy)
	}
	return mode, nil
}

func v2LaunchCWD(value string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	resolved, err := filepath.Abs(".")
	if err == nil {
		return resolved
	}
	return "."
}

func v2ReadPromptInputs(paths []string, prefix string, sourceAnchor string) ([]v2Input, error) {
	if len(paths) == 0 {
		return []v2Input{}, nil
	}
	seen := map[string]bool{}
	inputs := make([]v2Input, 0, len(paths))
	var total int64
	for index, rawPath := range paths {
		rawPath = strings.TrimSpace(rawPath)
		if rawPath == "" {
			return nil, fmt.Errorf("%s input path is empty", prefix)
		}
		filename := rawPath
		if !filepath.IsAbs(filename) && strings.TrimSpace(sourceAnchor) != "" {
			filename = filepath.Join(sourceAnchor, filename)
		}
		absolute, err := filepath.Abs(filename)
		if err != nil {
			return nil, err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("%s input %q cannot be normalized: %w", prefix, rawPath, err)
		}
		if seen[resolved] {
			return nil, fmt.Errorf("duplicate %s input after path normalization: %s", prefix, rawPath)
		}
		seen[resolved] = true
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("%s input %q is unreadable: %w", prefix, rawPath, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s input %q is not a regular text file", prefix, rawPath)
		}
		if info.Size() > v2PromptInputMaxBytes {
			return nil, fmt.Errorf("%s input %q exceeds %d bytes", prefix, rawPath, v2PromptInputMaxBytes)
		}
		total += info.Size()
		if total > v2PromptInputTotalBytes {
			return nil, fmt.Errorf("%s inputs exceed %d bytes", prefix, v2PromptInputTotalBytes)
		}
		body, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("read %s input %q: %w", prefix, rawPath, err)
		}
		if !utf8.Valid(body) || bytesContainNUL(body) {
			return nil, fmt.Errorf("%s input %q must be UTF-8 text", prefix, rawPath)
		}
		inputs = append(inputs, v2Input{Input: v2SessionInput(fmt.Sprintf("%s%d", prefix, index+1), body), Body: body})
	}
	return inputs, nil
}

func v2ReadNamedInputs(bindings []string, sourceAnchor string, limits recipes.RuntimeLimits) ([]namedinputs.Prepared, error) {
	values := make([]namedinputs.Binding, 0, len(bindings))
	for _, binding := range bindings {
		name, filename, found := strings.Cut(binding, "=")
		name = strings.TrimSpace(name)
		filename = strings.TrimSpace(filename)
		if !found || name == "" || filename == "" {
			return nil, fmt.Errorf("--input must be name=path")
		}
		values = append(values, namedinputs.Binding{Name: name, Path: filename})
	}
	return namedinputs.Read(values, sourceAnchor, namedinputs.Limits{MaxFileBytes: limits.NamedInputMaxBytes, MaxTotalBytes: limits.NamedInputTotalMaxBytes})
}

func v2SessionInput(name string, body []byte) session.Input {
	sum := sha256.Sum256(body)
	return session.Input{
		Name: name,
		Content: blobstore.BlobRef{
			SHA256:    hex.EncodeToString(sum[:]),
			Size:      int64(len(body)),
			MediaType: v2TextMediaType,
		},
	}
}

func bytesContainNUL(body []byte) bool {
	for _, value := range body {
		if value == 0 {
			return true
		}
	}
	return false
}

func v2Inputs(values []v2Input) []session.Input {
	items := make([]session.Input, 0, len(values))
	for _, value := range values {
		items = append(items, value.Input)
	}
	return items
}

func v2PersistInputs(sess *session.Session, groups ...[]v2Input) error {
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return err
	}
	for _, group := range groups {
		for _, value := range group {
			stored, err := blobs.PutBytes(value.Body, value.Input.Content.MediaType)
			if err != nil {
				return err
			}
			if !stored.Equal(value.Input.Content) {
				return fmt.Errorf("persisted input %q does not match compiled blob reference", value.Input.Name)
			}
		}
	}
	return nil
}

func v2TaskPlan(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

func v2RelayHome(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	if value = strings.TrimSpace(os.Getenv("CODEX_CLAUDE_HOME")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-claude"
	}
	return filepath.Join(home, ".codex-claude")
}

func v2SessionID(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return "session"
}

func v2CreateSession(relayHome, explicitDir string, value session.Plan) (*session.Session, error) {
	if strings.TrimSpace(explicitDir) == "" {
		home := filepath.Join(v2RelayHome(relayHome), "sessions")
		return session.CreateWithOptions(session.CreateOptions{RelayHome: home, Prefix: value.SessionID + "-", Plan: value})
	}
	target, err := filepath.Abs(explicitDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("session directory %q already exists", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	created, err := session.CreateWithOptions(session.CreateOptions{
		RelayHome: filepath.Dir(target),
		Prefix:    filepath.Base(target) + "-",
		Plan:      value,
	})
	if err != nil {
		return nil, err
	}
	if err := os.Rename(created.Root, target); err != nil {
		return nil, fmt.Errorf("publish explicit session directory: %w", err)
	}
	return session.Open(target)
}

func v2PrepareWorkspace(ctx context.Context, sess *session.Session, options v2WorkspaceOptions) (*workspace.Materialized, error) {
	launchCWD := strings.TrimSpace(options.LaunchCWD)
	if launchCWD == "" {
		var err error
		launchCWD, err = filepath.Abs(".")
		if err != nil {
			return nil, err
		}
	}
	return workspace.Prepare(ctx, sess, workspace.Options{LaunchCWD: launchCWD, Mode: options.Mode})
}

func v2Run(ctx context.Context, sess *session.Session, runtime relayv2.Runtime, executionCWD string, resume bool, prompt string, requestedTurns int) (map[string]any, error) {
	deps := relayv2.NewDeps(runtime, executionCWD)
	var runErr error
	if resume {
		_, runErr = engine.Resume(ctx, sess, deps, prompt, requestedTurns)
	} else {
		_, runErr = engine.Run(ctx, sess, deps)
	}
	report, reportErr := relayv2.BuildReport(sess, relayv2.ProjectionOptions{})
	if reportErr != nil {
		return nil, errors.Join(runErr, reportErr)
	}
	return report, runErr
}

func v2RecipePlan(
	config recipes.RuntimeConfig,
	recipeID string,
	sessionID string,
	task string,
	timeouts session.Timeouts,
	investigation string,
	contexts []v2Input,
	skills []v2Input,
	inputs []session.Input,
	taskPlan json.RawMessage,
	bundle *integration.Bundle,
) (session.Plan, []plan.Recipe, error) {
	recipe, err := relayv2.RecipeFromRuntime(config, recipeID)
	if err != nil {
		return session.Plan{}, nil, err
	}
	recipe.Inputs = append([]session.Input(nil), inputs...)
	if strings.TrimSpace(investigation) != "" {
		recipe.Investigation = investigation
	}
	if recipe.IntegrationContract != "" {
		schedule, err := integration.AlternatingSchedule(recipe.ParticipantTurns)
		if err != nil {
			return session.Plan{}, nil, err
		}
		selected, err := integration.SelectContract(bundle, recipe.IntegrationContract, integration.ScheduleRequirement{Turns: schedule, ResultSource: recipe.ResultSource})
		if err != nil {
			return session.Plan{}, nil, err
		}
		contract := selected.Contract()
		if contract != nil {
			recipe.Result.Format = contract.Result.Format
			if contract.Result.Schema != nil {
				body, err := json.Marshal(contract.Result.Schema)
				if err != nil {
					return session.Plan{}, nil, err
				}
				recipe.Result.Schema = body
			}
		}
		instructions, err := v2IntegrationInstructions(selected)
		if err != nil {
			return session.Plan{}, nil, err
		}
		compiled, err := plan.FromRecipe(plan.RecipeInput{
			SessionID: sessionID,
			Task:      task,
			Inline:    &recipe,
			Timeouts:  timeouts,
			Context:   v2Inputs(contexts),
			Skills:    v2Inputs(skills),
			TaskPlan:  taskPlan,
		})
		if err != nil {
			return session.Plan{}, nil, err
		}
		compiled.IntegrationInstructions = instructions
		if err := session.ValidatePlan(compiled); err != nil {
			return session.Plan{}, nil, err
		}
		catalog, err := relayv2.RecipesFromRuntime(config)
		if err != nil {
			return session.Plan{}, nil, err
		}
		return compiled, catalog, nil
	}
	compiled, err := plan.FromRecipe(plan.RecipeInput{
		SessionID: sessionID,
		Task:      task,
		Inline:    &recipe,
		Timeouts:  timeouts,
		Context:   v2Inputs(contexts),
		Skills:    v2Inputs(skills),
		TaskPlan:  taskPlan,
	})
	if err != nil {
		return session.Plan{}, nil, err
	}
	catalog, err := relayv2.RecipesFromRuntime(config)
	if err != nil {
		return session.Plan{}, nil, err
	}
	return compiled, catalog, nil
}

func v2IntegrationInstructions(selected *integration.SelectedContract) (*session.IntegrationInstructions, error) {
	if selected == nil {
		return nil, nil
	}
	contract := selected.Contract()
	if contract == nil {
		return nil, errors.New("selected integration contract has no contract body")
	}
	value := &session.IntegrationInstructions{
		Turns: make([]session.IntegrationTurn, 0, len(contract.Turns)),
	}
	for _, turn := range contract.Turns {
		value.Turns = append(value.Turns, session.IntegrationTurn{
			ParticipantTurn: turn.ParticipantTurn,
			Actor:           turn.Slot,
			Instructions:    turn.Instructions,
		})
	}
	if contract.Reducer != nil {
		value.ReducerInstructions = contract.Reducer.Instructions
	}
	return value, nil
}
