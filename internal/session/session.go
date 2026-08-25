// Package session owns the immutable v2 session.json plan and identity file.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
	"github.com/charlesnpx/convo-relay/internal/eventlog"
)

// Relay modes frame how actors address one another.
const (
	ModeAdversarial = "adversarial"
	ModeCooperative = "cooperative"
	ModeSteelman    = "steelman"
)

// Investigation levels control how much context an actor receives.
const (
	InvestigationAuto        = "auto"
	InvestigationNormal      = "normal"
	InvestigationContextOnly = "context_only"
)

// Provenance values. These label a plan's origin; they never select behaviour.
const (
	ProvenanceOrdinary = "ordinary"
	ProvenanceRecipe   = "recipe"
	ProvenanceChild    = "child"
)

const (
	PlanKind        = "relay.plan/v1"
	SchemaVersion   = 1
	SessionFilename = "session.json"
)

// Plan is the complete typed, portable compiled-plan document. It owns the
// only schema_version; nested plan records deliberately have none.
type Plan struct {
	Kind          string `json:"kind"`
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	// Provenance records where the plan came from: ordinary, recipe, or child.
	// It is a label for operators and inspectors. Nothing may branch on it to
	// choose execution behaviour - one plan, one engine.
	Provenance string `json:"provenance"`
	// RecipeID names the recipe a plan was compiled from, when it was. The
	// public run report surfaces it, so it is part of the operator contract.
	RecipeID string `json:"recipe_id,omitempty"`
	// Task is the operator's stated purpose for the run. It is deliberately a
	// plain string rather than a blob reference so the compiler stays a pure
	// function of its inputs and needs no blobstore. It is also the one field
	// excluded from the portability walk: it is operator-authored prose that may
	// legitimately name a path, and it reads identically on every machine, so it
	// is intentional content rather than incidental machine state.
	Task string `json:"task"`
	// Timeouts govern one provider turn and its stall watchdog. They are compiler
	// policy, fixed before this immutable boundary.
	Timeouts Timeouts `json:"timeouts"`
	// Mode frames how actors address each other: adversarial, cooperative, or
	// steelman. It shapes prompts and is recorded per turn, so it is execution
	// policy rather than presentation.
	Mode string `json:"mode"`
	// Investigation controls how much context an actor receives: auto, normal, or
	// context_only.
	Investigation string        `json:"investigation"`
	Actors        []Actor       `json:"actors"`
	Schedule      Schedule      `json:"schedule"`
	Facilitator   *Facilitator  `json:"facilitator,omitempty"`
	Reducer       *Reducer      `json:"reducer,omitempty"`
	ProviderRetry ProviderRetry `json:"provider_retry"`
	Workspace     Workspace     `json:"workspace"`
	Inputs        []Input       `json:"inputs"`
	// Context and Skills are the durable, blob-addressed forms of --context and
	// --skill. Their source paths deliberately do not enter the portable plan.
	Context       []Input         `json:"context"`
	Skills        []Input         `json:"skills"`
	TaskPlan      json.RawMessage `json:"task_plan,omitempty"`
	MatchKeywords []string        `json:"match_keywords"`
	ChildPolicy   ChildPolicy     `json:"child_policy"`
	Result        Result          `json:"result"`
	// Lifecycle carries recipe controls that do not select a second execution
	// path. Dynamic and workspace controls are also projected onto ChildPolicy
	// and Workspace by the compiler.
	Lifecycle           *Lifecycle `json:"lifecycle,omitempty"`
	IntegrationContract string     `json:"integration_contract,omitempty"`
	// IntegrationInstructions is the executable prompt projection of a selected
	// integration contract. It is immutable plan data rather than local runtime
	// configuration so retries and resume retain the same contract turn text.
	IntegrationInstructions *IntegrationInstructions `json:"integration_instructions,omitempty"`
}

type Actor struct {
	ID        string `json:"id"`
	Backend   string `json:"backend"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	ProfileID string `json:"profile_id,omitempty"`
}

type Schedule struct {
	Kind              string `json:"kind"`
	Turns             int    `json:"turns"`
	StopOnConvergence bool   `json:"stop_on_convergence"`
	// Order is the explicit per-turn actor sequence for a sequence schedule. It
	// exists so a fixed order is stated rather than inferred from the position of
	// entries in Actors. It must be empty for a dialogue schedule.
	Order []string `json:"order,omitempty"`
}

// Timeouts is measured in seconds. Zero is not valid: a plan states its own
// limits rather than letting a downstream default decide.
type Timeouts struct {
	TurnSeconds  int `json:"turn_seconds"`
	StallSeconds int `json:"stall_seconds"`
}

type Facilitator struct {
	Actor   string `json:"actor"`
	Cadence int    `json:"cadence"`
}

type Reducer struct {
	Actor string `json:"actor"`
}

// ProviderRetry is deliberately product-neutral: compiler policy chooses its
// values before this immutable boundary; runtime only consumes the typed form.
type ProviderRetry struct {
	Mode        string `json:"mode"`
	MaxAttempts int    `json:"max_attempts"`
}

type Workspace struct {
	Mode      string `json:"mode"`
	Isolation string `json:"isolation,omitempty"`
}

type Input struct {
	Name    string            `json:"name"`
	Content blobstore.BlobRef `json:"content"`
}

type ChildPolicy struct {
	Mode           string   `json:"mode"`
	MaxDepth       int      `json:"max_depth"`
	MaxChildren    int      `json:"max_children"`
	MaxTurns       int      `json:"max_turns"`
	AllowedRecipes []string `json:"allowed_recipes"`
}

type Result struct {
	Source string          `json:"source"`
	Format string          `json:"format"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// IntegrationInstructions contains only the contract text the engine needs
// while executing a selected integration-bound recipe. Bundle provenance and
// local source paths deliberately do not enter the portable plan.
type IntegrationInstructions struct {
	Turns               []IntegrationTurn `json:"turns,omitempty"`
	ReducerInstructions string            `json:"reducer_instructions,omitempty"`
}

// IntegrationTurn binds one selected contract instruction to the compiled
// participant turn and actor. ParticipantTurn is one-based, matching the
// dialogue schedule and integration contract declarations.
type IntegrationTurn struct {
	ParticipantTurn int    `json:"participant_turn"`
	Actor           string `json:"actor"`
	Instructions    string `json:"instructions"`
}

// Lifecycle is the canonical normalized recipe lifecycle projection. Runtime
// enforcement remains with the execution unit; the plan records the policy
// that it must enforce.
type Lifecycle struct {
	Resume             string `json:"resume"`
	Steering           string `json:"steering"`
	Dynamic            string `json:"dynamic"`
	WorkspaceIsolation string `json:"workspace_isolation"`
}

// Session is a decoded immutable plan plus its machine-local directory. Root
// is never emitted by Plan's JSON representation.
type Session struct {
	Root   string
	Plan   Plan
	Digest string
}

// CreateOptions controls managed-root creation. There is intentionally no
// target-session-directory field: Create always uses os.MkdirTemp below
// RelayHome and cannot initialize a caller-supplied existing directory.
type CreateOptions struct {
	RelayHome  string
	Prefix     string
	Plan       Plan
	BlobLimits blobstore.Limits
}

// Create makes a new managed session under relayHome using the default prefix.
func Create(relayHome string, plan Plan) (*Session, error) {
	return CreateWithOptions(CreateOptions{RelayHome: relayHome, Plan: plan})
}

// CreateWithOptions makes a new managed session and writes session.json once.
func CreateWithOptions(options CreateOptions) (*Session, error) {
	relayHome := strings.TrimSpace(options.RelayHome)
	if relayHome == "" {
		return nil, errors.New("relay home is required")
	}
	if err := os.MkdirAll(relayHome, 0o700); err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(options.Prefix)
	if prefix == "" {
		prefix = "session-"
	}
	root, err := os.MkdirTemp(relayHome, prefix)
	if err != nil {
		return nil, err
	}

	plan := options.Plan
	if plan.SessionID == "" {
		plan.SessionID = filepath.Base(root)
	}
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	if err := eventlog.ValidatePortableValue(portableProjection(plan), root, relayHome); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		return nil, err
	}
	if _, err := blobstore.New(root, options.BlobLimits); err != nil {
		return nil, err
	}
	body, err := eventlog.SemanticJSONBytes(plan)
	if err != nil {
		return nil, err
	}
	if err := writeSessionOnce(root, body); err != nil {
		return nil, err
	}
	if err := eventlog.Initialize(root); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	digest, err := eventlog.SemanticJSONDigestBytes(body)
	if err != nil {
		return nil, err
	}
	return &Session{Root: root, Plan: plan, Digest: digest}, nil
}

func writeSessionOnce(root string, body []byte) error {
	filename := filepath.Join(root, SessionFilename)
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeAll(file, body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Open reads a session.json file, verifies it is strict semantic JSON, checks
// its typed shape, and returns its semantic-json digest.
func Open(root string) (*Session, error) {
	cleaned := filepath.Clean(root)
	filename := filepath.Join(cleaned, SessionFilename)
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("session.json must be a regular file")
	}
	body, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := eventlog.DecodeCanonicalJSON(body, &plan); err != nil {
		return nil, err
	}
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	if err := eventlog.ValidatePortableValue(portableProjection(plan), cleaned); err != nil {
		return nil, err
	}
	digest, err := eventlog.SemanticJSONDigestBytes(body)
	if err != nil {
		return nil, err
	}
	return &Session{Root: cleaned, Plan: plan, Digest: digest}, nil
}

// BlobStore opens the session's physical blob store without creating a second
// authority or exposing its root in Plan.
func (s *Session) BlobStore(limits blobstore.Limits) (*blobstore.Store, error) {
	if s == nil {
		return nil, errors.New("nil session")
	}
	return blobstore.Open(s.Root, limits)
}

// EventWriter opens the append-only authority. Supplying the blob store is
// required for payload-bearing events, enforcing blob-before-event ordering.
func (s *Session) EventWriter(blobs eventlog.BlobVerifier) (*eventlog.Writer, error) {
	if s == nil {
		return nil, errors.New("nil session")
	}
	return eventlog.OpenWriter(s.Root, blobs)
}

// PlanDigest computes the semantic-json digest that session.started binds.
func PlanDigest(plan Plan) (string, error) {
	if err := ValidatePlan(plan); err != nil {
		return "", err
	}
	return eventlog.SemanticJSONDigest(plan)
}

// BlobRefs returns every plan-input reference, for bundle closure and sweep.
func BlobRefs(plan Plan) []blobstore.BlobRef {
	refs := make([]blobstore.BlobRef, 0, len(plan.Inputs)+len(plan.Context)+len(plan.Skills))
	for _, inputs := range [][]Input{plan.Inputs, plan.Context, plan.Skills} {
		for _, input := range inputs {
			refs = append(refs, input.Content)
		}
	}
	return refs
}

// ValidateEventBindings checks the optional session.started binding when it is
// present in a log. Empty, newly-created logs remain valid; a recorded start
// must bind this exact immutable plan digest and session identity.
func ValidateEventBindings(plan Plan, events []eventlog.Event) error {
	digest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	for _, event := range events {
		var started eventlog.SessionStartedPayload
		switch payload := event.Payload.(type) {
		case eventlog.SessionStartedPayload:
			started = payload
		case *eventlog.SessionStartedPayload:
			if payload == nil {
				continue
			}
			started = *payload
		default:
			continue
		}
		if started.PlanDigest != digest {
			return errors.New("session.started plan_digest does not bind session.json")
		}
		if started.SessionID != plan.SessionID {
			return errors.New("session.started session_id does not bind session.json")
		}
	}
	return nil
}

// ValidatePlan is the sole typed boundary validator for compiled plans.
func ValidatePlan(plan Plan) error {
	if plan.Kind != PlanKind {
		return fmt.Errorf("plan kind must be %s", PlanKind)
	}
	if plan.SchemaVersion != SchemaVersion {
		return fmt.Errorf("plan schema_version must be %d", SchemaVersion)
	}
	switch plan.Provenance {
	case ProvenanceOrdinary, ProvenanceRecipe, ProvenanceChild:
	default:
		return fmt.Errorf("plan provenance must be one of %s, %s, %s", ProvenanceOrdinary, ProvenanceRecipe, ProvenanceChild)
	}
	if (plan.Provenance == ProvenanceRecipe || plan.Provenance == ProvenanceChild) && plan.RecipeID == "" {
		return errors.New("plan compiled from a recipe must record recipe_id")
	}
	if strings.TrimSpace(plan.Task) == "" {
		return errors.New("plan must state a task")
	}
	if plan.Timeouts.TurnSeconds <= 0 {
		return errors.New("plan timeouts.turn_seconds must be positive")
	}
	if plan.Timeouts.StallSeconds <= 0 {
		return errors.New("plan timeouts.stall_seconds must be positive")
	}
	switch plan.Mode {
	case ModeAdversarial, ModeCooperative, ModeSteelman:
	default:
		return fmt.Errorf("plan mode must be one of %s, %s, %s", ModeAdversarial, ModeCooperative, ModeSteelman)
	}
	switch plan.Investigation {
	case InvestigationAuto, InvestigationNormal, InvestigationContextOnly:
	default:
		return fmt.Errorf("plan investigation must be one of %s, %s, %s", InvestigationAuto, InvestigationNormal, InvestigationContextOnly)
	}
	if err := validateToken("session_id", plan.SessionID); err != nil {
		return err
	}
	if len(plan.Actors) == 0 {
		return errors.New("plan must contain at least one actor")
	}
	actorIDs := make(map[string]bool, len(plan.Actors))
	for _, actor := range plan.Actors {
		if err := validateToken("actor.id", actor.ID); err != nil {
			return err
		}
		if _, exists := actorIDs[actor.ID]; exists {
			return fmt.Errorf("plan contains duplicate actor %q", actor.ID)
		}
		actorIDs[actor.ID] = false
		switch actor.Backend {
		case "claude", "codex", "gemini":
		default:
			return fmt.Errorf("actor backend %q is not supported", actor.Backend)
		}
		if err := validateOptionalToken("actor.model", actor.Model); err != nil {
			return err
		}
		if err := validateOptionalToken("actor.effort", actor.Effort); err != nil {
			return err
		}
		if err := validateOptionalToken("actor.profile_id", actor.ProfileID); err != nil {
			return err
		}
	}
	if plan.Schedule.Turns < 1 {
		return errors.New("schedule turns must be positive")
	}
	if plan.Facilitator != nil {
		if _, exists := actorIDs[plan.Facilitator.Actor]; !exists {
			return errors.New("facilitator actor is not in actors")
		}
		if plan.Facilitator.Cadence < 1 {
			return errors.New("facilitator cadence must be positive")
		}
	}
	if plan.Reducer != nil {
		if _, exists := actorIDs[plan.Reducer.Actor]; !exists {
			return errors.New("reducer actor is not in actors")
		}
	}
	controlActorCount := 0
	if plan.Facilitator != nil {
		controlActorCount++
	}
	if plan.Reducer != nil && (plan.Facilitator == nil || plan.Reducer.Actor != plan.Facilitator.Actor) {
		controlActorCount++
	}
	switch plan.Schedule.Kind {
	case "dialogue":
		if len(plan.Schedule.Order) != 0 {
			return errors.New("schedule.order is only valid for a sequence schedule")
		}
		if participants := len(plan.Actors) - controlActorCount; participants != 2 {
			return fmt.Errorf("dialogue schedule requires exactly two participants, got %d", participants)
		}
	case "sequence":
		if len(plan.Schedule.Order) != plan.Schedule.Turns {
			return fmt.Errorf("sequence schedule order must contain exactly %d entries", plan.Schedule.Turns)
		}
		scheduledActorCount := 0
		for _, actorID := range plan.Schedule.Order {
			scheduled, exists := actorIDs[actorID]
			if !exists {
				return fmt.Errorf("schedule.order names unknown actor %q", actorID)
			}
			if (plan.Facilitator != nil && plan.Facilitator.Actor == actorID) || (plan.Reducer != nil && plan.Reducer.Actor == actorID) {
				return fmt.Errorf("schedule.order must not name control actor %q", actorID)
			}
			if !scheduled {
				actorIDs[actorID] = true
				scheduledActorCount++
			}
		}
		if scheduledActorCount != len(plan.Actors)-controlActorCount {
			return errors.New("sequence schedule must include every actor that does not hold a control role")
		}
	default:
		return errors.New("schedule kind must be dialogue or sequence")
	}
	if plan.ProviderRetry.Mode != "allow" && plan.ProviderRetry.Mode != "forbid" {
		return errors.New("provider_retry mode must be allow or forbid")
	}
	if plan.ProviderRetry.MaxAttempts < 1 {
		return errors.New("provider_retry max_attempts must be positive")
	}
	if plan.ProviderRetry.Mode == "forbid" && plan.ProviderRetry.MaxAttempts != 1 {
		return errors.New("provider_retry forbid mode requires exactly one attempt")
	}
	if plan.Workspace.Mode != "current" && plan.Workspace.Mode != "head-copy" {
		return errors.New("workspace mode must be current or head-copy")
	}
	if plan.Workspace.Isolation != "" && plan.Workspace.Isolation != "inherited" && plan.Workspace.Isolation != "read_only" && plan.Workspace.Isolation != "ephemeral" {
		return errors.New("workspace isolation must be inherited, read_only, or ephemeral")
	}
	for _, group := range []struct {
		label  string
		inputs []Input
	}{
		{label: "input", inputs: plan.Inputs},
		{label: "context", inputs: plan.Context},
		{label: "skill", inputs: plan.Skills},
	} {
		if err := validateInputs(group.label, group.inputs); err != nil {
			return err
		}
	}
	if err := validateUniqueTokens("match_keywords", plan.MatchKeywords); err != nil {
		return err
	}
	if len(plan.TaskPlan) > 0 {
		if _, err := eventlog.SemanticJSONBytesRaw(plan.TaskPlan); err != nil {
			return fmt.Errorf("task_plan is not strict JSON: %w", err)
		}
	}
	switch plan.ChildPolicy.Mode {
	case "deny", "ask", "allow":
	default:
		return errors.New("child_policy mode must be deny, ask, or allow")
	}
	if plan.ChildPolicy.MaxDepth < 0 || plan.ChildPolicy.MaxChildren < 0 || plan.ChildPolicy.MaxTurns < 0 {
		return errors.New("child policy limits must not be negative")
	}
	if err := validateUniqueTokens("child_policy.allowed_recipes", plan.ChildPolicy.AllowedRecipes); err != nil {
		return err
	}
	switch plan.Result.Source {
	case "last_turn", "reducer":
	default:
		return errors.New("result source must be last_turn or reducer")
	}
	if plan.Result.Source == "reducer" && plan.Reducer == nil {
		return errors.New("reducer result source requires a reducer")
	}
	if err := validateToken("result.format", plan.Result.Format); err != nil {
		return err
	}
	if len(plan.Result.Schema) > 0 {
		if _, err := eventlog.SemanticJSONBytesRaw(plan.Result.Schema); err != nil {
			return fmt.Errorf("result schema is not strict JSON: %w", err)
		}
	}
	if err := validateLifecycle(plan.Lifecycle); err != nil {
		return err
	}
	if plan.Lifecycle != nil && plan.Lifecycle.Dynamic == "forbid" && plan.ChildPolicy.Mode != "deny" {
		return errors.New("lifecycle dynamic forbid requires child_policy mode deny")
	}
	if plan.Lifecycle != nil && plan.Lifecycle.Resume == "forbid" && plan.ChildPolicy.Mode == "ask" {
		return errors.New("lifecycle.resume forbid is incompatible with child_policy.mode ask")
	}
	// A recipe's declared workspace isolation is a minimum. The executable
	// workspace may strengthen it but never weaken it, per the documented
	// operator contract. Validated here because the plan is the only place both
	// values are visible: the compiler emits consistent pairs, but Create and
	// Open would otherwise persist a contradiction the engine cannot execute
	// unambiguously - it would have to choose between the recorded minimum and
	// the weaker executable value.
	if plan.Lifecycle != nil {
		minimum, minimumKnown := workspaceIsolationRank(plan.Lifecycle.WorkspaceIsolation)
		effective, effectiveKnown := workspaceIsolationRank(plan.Workspace.Isolation)
		if minimumKnown && effectiveKnown && effective < minimum {
			return fmt.Errorf("workspace isolation %q weakens the recipe minimum %q", plan.Workspace.Isolation, plan.Lifecycle.WorkspaceIsolation)
		}
	}
	if plan.IntegrationContract != "" {
		if err := validateToken("integration_contract", plan.IntegrationContract); err != nil {
			return err
		}
	}
	if err := validateIntegrationInstructions(plan, actorIDs); err != nil {
		return err
	}
	return eventlog.ValidatePortableValue(portableProjection(plan))
}

func validateIntegrationInstructions(plan Plan, actors map[string]bool) error {
	instructions := plan.IntegrationInstructions
	if instructions == nil {
		return nil
	}
	if plan.IntegrationContract == "" {
		return errors.New("integration instructions require integration_contract")
	}
	turns := make(map[int]struct{}, len(instructions.Turns))
	for _, turn := range instructions.Turns {
		if turn.ParticipantTurn < 1 {
			return errors.New("integration instruction participant_turn must be positive")
		}
		if _, exists := turns[turn.ParticipantTurn]; exists {
			return fmt.Errorf("plan contains duplicate integration instruction for participant turn %d", turn.ParticipantTurn)
		}
		turns[turn.ParticipantTurn] = struct{}{}
		if _, exists := actors[turn.Actor]; !exists {
			return fmt.Errorf("integration instruction names unknown actor %q", turn.Actor)
		}
		if (plan.Facilitator != nil && turn.Actor == plan.Facilitator.Actor) ||
			(plan.Reducer != nil && turn.Actor == plan.Reducer.Actor) {
			return fmt.Errorf("integration instruction must name a participant actor, got %q", turn.Actor)
		}
		if strings.TrimSpace(turn.Instructions) == "" || strings.Contains(turn.Instructions, "\x00") {
			return fmt.Errorf("integration instruction for participant turn %d is invalid", turn.ParticipantTurn)
		}
	}
	if instructions.ReducerInstructions != "" {
		if plan.Reducer == nil {
			return errors.New("integration reducer instructions require a reducer")
		}
		if strings.Contains(instructions.ReducerInstructions, "\x00") {
			return errors.New("integration reducer instructions contain a control character")
		}
	}
	if plan.Result.Source == "reducer" && strings.TrimSpace(instructions.ReducerInstructions) == "" {
		return errors.New("integration reducer result requires reducer instructions")
	}
	return nil
}

// ActorIDs returns deterministic actor ids without exposing an untyped map.
func ActorIDs(plan Plan) []string {
	ids := make([]string, 0, len(plan.Actors))
	for _, actor := range plan.Actors {
		ids = append(ids, actor.ID)
	}
	sort.Strings(ids)
	return ids
}

func validateToken(label string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a control character", label)
	}
	return nil
}

func validateOptionalToken(label string, value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a control character", label)
	}
	return nil
}

func validateInputs(label string, inputs []Input) error {
	names := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if err := validateLogicalName(input.Name); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if _, exists := names[input.Name]; exists {
			return fmt.Errorf("plan contains duplicate %s %q", label, input.Name)
		}
		names[input.Name] = struct{}{}
		if err := blobstore.ValidateRef(input.Content); err != nil {
			return err
		}
	}
	return nil
}

func validateUniqueTokens(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateToken(label, value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("plan contains duplicate %s %q", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateLifecycle(value *Lifecycle) error {
	if value == nil {
		return nil
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "lifecycle.resume", value: value.Resume},
		{label: "lifecycle.steering", value: value.Steering},
		{label: "lifecycle.dynamic", value: value.Dynamic},
	} {
		if field.value != "allow" && field.value != "forbid" {
			return fmt.Errorf("%s must be allow or forbid", field.label)
		}
	}
	switch value.WorkspaceIsolation {
	case "inherited", "read_only", "ephemeral":
		return nil
	default:
		return errors.New("lifecycle.workspace_isolation must be inherited, read_only, or ephemeral")
	}
}

func validateLogicalName(value string) error {
	if err := validateToken("input.name", value); err != nil {
		return err
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || value == "." || value == ".." {
		return errors.New("input.name must not be a path")
	}
	return nil
}

func writeAll(file *os.File, body []byte) error {
	for len(body) > 0 {
		count, err := file.Write(body)
		if err != nil {
			return err
		}
		if count == 0 {
			return errors.New("short session.json write")
		}
		body = body[count:]
	}
	return nil
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// CanonicalBytes returns the immutable session.json bytes a Create call writes.
func CanonicalBytes(plan Plan) ([]byte, error) {
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	return eventlog.SemanticJSONBytes(plan)
}

// Equal reports semantic equality of two typed plans, including exact-decimal
// schema bytes after canonicalization.
func (plan Plan) Equal(other Plan) bool {
	left, leftErr := CanonicalBytes(plan)
	right, rightErr := CanonicalBytes(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// portableProjection returns the plan with Task cleared, for the portability walk
// only. Task is operator-authored prose that may deliberately name a path; it is
// identical on every machine, so it cannot break relocation of a bundle. Every
// other field stays bound, including the recipe id and every input name.
func portableProjection(plan Plan) Plan {
	plan.Task = ""
	return plan
}

// workspaceIsolationRank orders the isolation policies weakest to strongest,
// matching internal/workspace.policyRank, which is the authority. An empty or
// unrecognised value reports unknown so the enum checks own that rejection.
func workspaceIsolationRank(value string) (int, bool) {
	switch value {
	case "inherited":
		return 0, true
	case "read_only":
		return 1, true
	case "ephemeral":
		return 2, true
	default:
		return 0, false
	}
}
