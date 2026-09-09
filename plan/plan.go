// Package plan defines the portable execution document accepted by
// convo-relay run --plan.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/semanticjson"
)

const (
	// PlanKind is the required value of Plan.Kind; any other kind makes a plan
	// invalid.
	PlanKind = "relay.plan/v1"
	// SchemaVersion is the required value of Plan.SchemaVersion; any other
	// version makes a plan invalid.
	SchemaVersion = 1

	// ModeAdversarial makes actors challenge one another's claims; using any
	// other mode string makes Plan invalid.
	ModeAdversarial = "adversarial"
	// ModeCooperative makes actors develop an answer together; using any other
	// mode string makes Plan invalid.
	ModeCooperative = "cooperative"
	// ModeSteelman asks actors to strengthen the position under discussion;
	// using any other mode string makes Plan invalid.
	ModeSteelman = "steelman"

	// InvestigationAuto lets the runtime choose the normal investigation scope;
	// using any other investigation string makes Plan invalid.
	InvestigationAuto = "auto"
	// InvestigationNormal gives an actor the normal investigation context; using
	// any other investigation string makes Plan invalid.
	InvestigationNormal = "normal"
	// InvestigationContextOnly restricts an actor to supplied context; using any
	// other investigation string makes Plan invalid.
	InvestigationContextOnly = "context_only"

	// ProvenanceOrdinary labels a plan compiled from ordinary launch settings;
	// using any other provenance string makes Plan invalid.
	ProvenanceOrdinary = "ordinary"
	// ProvenanceRecipe labels a plan compiled from a recipe; it is invalid unless
	// RecipeID is also set.
	ProvenanceRecipe = "recipe"
	// ProvenanceChild labels a plan compiled as a child execution; it is invalid
	// unless RecipeID is also set.
	ProvenanceChild = "child"
	// ProvenanceSupplied labels a plan supplied directly by a caller; it is
	// invalid when used with an unsupported provenance value.
	ProvenanceSupplied = "supplied"

	// ScheduleDialogue names the two-participant dialogue schedule; a schedule
	// with another kind or the wrong participant count is invalid.
	ScheduleDialogue = "dialogue"
	// ScheduleSequence names the explicit ordered sequence schedule; a schedule
	// with another kind or an incomplete order is invalid.
	ScheduleSequence = "sequence"

	// ProviderRetryAllow permits retries up to ProviderRetry.MaxAttempts; using it
	// with a non-positive attempt limit makes ProviderRetry invalid.
	ProviderRetryAllow = "allow"
	// ProviderRetryForbid permits exactly one provider attempt; another attempt
	// limit makes ProviderRetry invalid.
	ProviderRetryForbid = "forbid"

	// WorkspaceModeCurrent executes against the supplied working directory; any
	// other workspace mode makes Workspace invalid.
	WorkspaceModeCurrent = "current"
	// WorkspaceModeHeadCopy executes against a copy of the committed HEAD tree;
	// any other workspace mode makes Workspace invalid.
	WorkspaceModeHeadCopy = "head-copy"

	// ChildPolicyDeny disallows child executions; an unsupported policy mode makes
	// ChildPolicy invalid.
	ChildPolicyDeny = "deny"
	// ChildPolicyAsk requires an operator decision before a child execution; an
	// unsupported policy mode makes ChildPolicy invalid.
	ChildPolicyAsk = "ask"
	// ChildPolicyAllow admits child executions within the declared limits; an
	// unsupported policy mode makes ChildPolicy invalid.
	ChildPolicyAllow = "allow"

	// ResultSourceLastTurn takes the result from the last participant turn; using
	// another source string makes Result invalid.
	ResultSourceLastTurn = "last_turn"
	// ResultSourceReducer takes the result from the reducer; it is invalid when
	// the plan has no reducer.
	ResultSourceReducer = "reducer"

	// ResultFormatText declares a text result; using another format string makes
	// Result invalid.
	ResultFormatText = "text"
	// ResultFormatJSON declares a JSON result; using another format string makes
	// Result invalid.
	ResultFormatJSON = "json"

	// ActorBackendClaude selects the Claude provider backend; an unsupported
	// backend string makes Actor invalid.
	ActorBackendClaude = "claude"
	// ActorBackendCodex selects the Codex provider backend; an unsupported
	// backend string makes Actor invalid.
	ActorBackendCodex = "codex"
	// ActorBackendGemini selects the Gemini provider backend; an unsupported
	// backend string makes Actor invalid.
	ActorBackendGemini = "gemini"
	// ActorBackendChild selects a compiled child recipe step; it is invalid when
	// ChildRecipeID is absent or ChildTurns is negative.
	ActorBackendChild = "child"

	// LifecycleAllow permits the corresponding lifecycle operation; another
	// lifecycle value makes Lifecycle invalid.
	LifecycleAllow = "allow"
	// LifecycleForbid forbids the corresponding lifecycle operation; another
	// lifecycle value makes Lifecycle invalid.
	LifecycleForbid = "forbid"
)

// BlobRef identifies one raw payload in the content-addressed blob store.
// A reference is invalid when SHA256 is not 64 lower-case hexadecimal
// characters, Size is negative, MediaType is empty, or MediaType contains a
// control character.
type BlobRef struct {
	// SHA256 is the lower-case hexadecimal SHA-256 digest of the raw payload.
	SHA256 string `json:"sha256"`
	// Size is the payload size in bytes and must not be negative.
	Size int64 `json:"size"`
	// MediaType describes the payload bytes and must be a non-empty value without
	// carriage returns, newlines, or NUL.
	MediaType string `json:"media_type"`
}

// Equal reports whether two references identify the same payload and metadata.
// It returns false when any field differs; it does not validate either
// reference, so malformed references are only equal to the same malformed
// value.
func (value BlobRef) Equal(other BlobRef) bool {
	return value.SHA256 == other.SHA256 && value.Size == other.Size && value.MediaType == other.MediaType
}

// Plan is the complete typed, portable execution document. It is invalid when
// any required enum, identity, relationship, limit, payload reference, raw
// JSON value, lifecycle rule, or portability rule is violated by Validate.
type Plan struct {
	// Kind identifies this document family and must equal PlanKind; another value
	// makes the plan invalid.
	Kind string `json:"kind"`
	// SchemaVersion identifies the document schema and must equal SchemaVersion;
	// another value makes the plan invalid.
	SchemaVersion int `json:"schema_version"`
	// SessionID is the stable session identity and must be a non-empty token
	// without carriage returns, newlines, or NUL; otherwise the plan is invalid.
	SessionID string `json:"session_id"`
	// Provenance labels how the plan was created and must be one of the
	// Provenance constants. Recipe and child plans must also set RecipeID;
	// another value or a missing required RecipeID makes the plan invalid.
	Provenance string `json:"provenance"`
	// RecipeID identifies the source recipe when Provenance is recipe or child;
	// it is optional for other provenance values and is invalid when required but
	// empty.
	RecipeID string `json:"recipe_id,omitempty"`
	// Task is the operator's purpose for the run and must contain non-whitespace
	// text. It is the only plan text excluded from portability checks; empty or
	// whitespace-only text makes the plan invalid.
	Task string `json:"task"`
	// Timeouts fixes the provider-turn and stall-watchdog limits; both values
	// must be positive seconds or the plan is invalid.
	Timeouts Timeouts `json:"timeouts"`
	// Mode controls how actors address one another and must be a Mode constant;
	// another value makes the plan invalid.
	Mode string `json:"mode"`
	// Investigation controls the context given to actors and must be an
	// Investigation constant; another value makes the plan invalid.
	Investigation string `json:"investigation"`
	// Actors lists every participant and control actor; it must not be empty, IDs
	// must be unique, and backend-specific fields must be valid.
	Actors []Actor `json:"actors"`
	// Schedule fixes the number and order of participant turns; invalid kind,
	// turn count, or order makes the plan invalid.
	Schedule Schedule `json:"schedule"`
	// Facilitator identifies an optional non-child actor that records the ledger;
	// an unknown or child actor, or a non-positive cadence, makes the plan invalid.
	Facilitator *Facilitator `json:"facilitator,omitempty"`
	// Reducer identifies an optional non-child actor that produces a reduced
	// result; an unknown or child actor makes the plan invalid.
	Reducer *Reducer `json:"reducer,omitempty"`
	// ProviderRetry fixes whether provider failures may be retried; unsupported
	// mode, non-positive attempts, or a forbidden retry count makes it invalid.
	ProviderRetry ProviderRetry `json:"provider_retry"`
	// Workspace fixes the machine-local execution workspace mode; an unsupported
	// mode makes the plan invalid.
	Workspace Workspace `json:"workspace"`
	// Inputs contains named execution inputs; names and each non-empty BlobRef
	// list must be valid or the plan is invalid.
	Inputs []Input `json:"inputs"`
	// Context contains durable context payloads; names and each non-empty BlobRef
	// list must be valid or the plan is invalid.
	Context []Input `json:"context"`
	// Skills contains durable skill payloads; names and each non-empty BlobRef
	// list must be valid or the plan is invalid.
	Skills []Input `json:"skills"`
	// TaskPlan contains optional strict JSON for the task-specific plan; malformed
	// or non-strict JSON makes the plan invalid.
	TaskPlan json.RawMessage `json:"task_plan,omitempty"`
	// ChildPolicy fixes child admission and resource limits; unsupported modes,
	// negative limits, duplicate IDs, or invalid recipe tokens make it invalid.
	ChildPolicy ChildPolicy `json:"child_policy"`
	// Result fixes the source and representation of the run result; unsupported
	// source or format, a missing reducer, or malformed schema makes it invalid.
	Result Result `json:"result"`
	// Lifecycle carries optional resume, steering, and dynamic-expansion policy;
	// unsupported values or contradictions with ChildPolicy make the plan invalid.
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
	// Instructions carries optional immutable participant and reducer instruction
	// text; invalid turns, actors, or text make the plan invalid.
	Instructions *Instructions `json:"instructions,omitempty"`
}

// UnmarshalJSON decodes semantic-JSON numbers into Plan's integer fields while
// retaining strict unknown-field rejection. It returns an error for malformed
// JSON, unknown fields, or values that cannot be decoded into Plan.
func (value *Plan) UnmarshalJSON(body []byte) error {
	type plainPlan Plan
	var decoded plainPlan
	if err := semanticjson.DecodeJSON(body, &decoded); err != nil {
		return err
	}
	*value = Plan(decoded)
	return nil
}

// Actor identifies one provider or compiled child step. It is invalid when ID
// is empty or duplicated, Backend is unsupported, or provider/child-specific
// fields violate the selected backend.
type Actor struct {
	// ID is the unique actor identifier used by schedules and control roles; it
	// must be non-empty, control-free, and unique or the plan is invalid.
	ID string `json:"id"`
	// Backend selects a provider or the child backend and must be an actor
	// backend constant; another value makes the plan invalid.
	Backend string `json:"backend"`
	// Model optionally selects a provider model and must not contain control
	// characters; a control character makes the plan invalid.
	Model string `json:"model"`
	// Effort optionally selects provider effort and must not contain control
	// characters; a control character makes the plan invalid.
	Effort string `json:"effort"`
	// ProfileID optionally records the provider profile and must not contain
	// control characters; a control character makes the plan invalid.
	ProfileID string `json:"profile_id,omitempty"`
	// ChildRecipeID identifies a child recipe when Backend is child and is
	// required for that backend; a missing or control-bearing value makes the
	// actor invalid.
	ChildRecipeID string `json:"child_recipe_id,omitempty"`
	// ChildTurns caps turns for a child actor and must not be negative; a
	// negative value makes the actor invalid.
	ChildTurns int `json:"child_turns,omitempty"`
}

// Schedule fixes the participant turn model. It is invalid when Turns is not
// positive, Kind is unsupported, or Order does not match a sequence schedule.
type Schedule struct {
	// Kind selects ScheduleDialogue or ScheduleSequence; another value makes the
	// schedule invalid.
	Kind string `json:"kind"`
	// Turns is the positive participant-turn count; a non-positive value makes
	// the schedule invalid.
	Turns int `json:"turns"`
	// StopOnConvergence permits the runtime's convergence stop rule; the zero
	// value is valid and this flag cannot by itself make a schedule invalid.
	StopOnConvergence bool `json:"stop_on_convergence"`
	// Order is the explicit per-turn actor sequence; it must be empty for a
	// dialogue and contain every non-control actor for a sequence or the schedule
	// is invalid.
	Order []string `json:"order,omitempty"`
}

// Timeouts fixes provider and stall watchdog durations in seconds. Both fields
// are invalid when they are zero or negative.
type Timeouts struct {
	// TurnSeconds is the maximum duration of one provider turn; a non-positive
	// value makes the plan invalid.
	TurnSeconds int `json:"turn_seconds"`
	// StallSeconds is the maximum provider output stall duration; a non-positive
	// value makes the plan invalid.
	StallSeconds int `json:"stall_seconds"`
}

// Facilitator identifies the actor that periodically records the conversation
// ledger. It is invalid when Actor is not a non-child actor or Cadence is not
// positive.
type Facilitator struct {
	// Actor names the facilitator actor; it must name a non-child actor or the
	// plan is invalid.
	Actor string `json:"actor"`
	// Cadence is the positive participant-turn interval for facilitation; a
	// non-positive value makes the plan invalid.
	Cadence int `json:"cadence"`
}

// Reducer identifies the actor that produces a reduced result. It is invalid
// when Actor is not a non-child actor.
type Reducer struct {
	// Actor names the reducer actor; it must name a non-child actor or the plan is
	// invalid.
	Actor string `json:"actor"`
}

// ProviderRetry fixes provider retry behavior. It is invalid when Mode is not
// a provider-retry constant, MaxAttempts is not positive, or forbid mode does
// not use exactly one attempt.
type ProviderRetry struct {
	// Mode selects whether provider retries are allowed; an unsupported value
	// makes ProviderRetry invalid.
	Mode string `json:"mode"`
	// MaxAttempts is the positive total attempt limit; a non-positive value or a
	// value other than one in forbid mode makes ProviderRetry invalid.
	MaxAttempts int `json:"max_attempts"`
}

// Workspace fixes the execution workspace. It is invalid when Mode is not a
// workspace mode constant.
type Workspace struct {
	// Mode selects current or head-copy execution; another value makes Workspace
	// invalid.
	Mode string `json:"mode"`
}

// Input names an ordered group of payloads. It is invalid when Name is empty,
// path-like, duplicated within its group, or Contents is empty or malformed.
type Input struct {
	// Name is the logical input name and must be non-empty, path-free, and unique
	// within its group or the plan is invalid.
	Name string `json:"name"`
	// Contents is the ordered list of payload references for this input; it must
	// be non-empty and contain valid BlobRefs or the plan is invalid.
	Contents []BlobRef `json:"contents"`
}

// ChildPolicy fixes child admission and resource limits. It is invalid when
// Mode is unsupported, a limit is negative, or AllowedRecipes has duplicates
// or invalid tokens.
type ChildPolicy struct {
	// Mode selects deny, ask, or allow admission; another value makes
	// ChildPolicy invalid.
	Mode string `json:"mode"`
	// MaxDepth is the non-negative remaining child nesting limit; a negative
	// value makes ChildPolicy invalid.
	MaxDepth int `json:"max_depth"`
	// MaxChildren is the non-negative child-count limit; a negative value makes
	// ChildPolicy invalid.
	MaxChildren int `json:"max_children"`
	// MaxTurns is the non-negative child-turn limit; a negative value makes
	// ChildPolicy invalid.
	MaxTurns int `json:"max_turns"`
	// AllowedRecipes lists optional unique recipe IDs allowed for children; a
	// duplicate, empty, or control-bearing ID makes ChildPolicy invalid.
	AllowedRecipes []string `json:"allowed_recipes"`
}

// Result fixes where and how the run result is produced. It is invalid
// when Source or Format is unsupported, a reducer is absent for reducer source,
// or Schema is not strict JSON.
type Result struct {
	// Source selects the last participant turn or reducer result; an unsupported
	// value or absent reducer for reducer results makes Result invalid.
	Source string `json:"source"`
	// Format selects text or JSON result encoding; an unsupported value makes
	// Result invalid.
	Format string `json:"format"`
	// Schema optionally contains strict JSON describing a JSON result; malformed
	// or non-strict JSON makes Result invalid.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// Instructions contains immutable prompt text for participant turns and the
// reducer. It is invalid when a turn is out of range, duplicated, names a
// control actor, or contains empty/control text.
type Instructions struct {
	// Turns binds instruction text to participant turns; an out-of-range,
	// duplicate, or invalid instruction makes Instructions invalid.
	Turns []TurnInstruction `json:"turns,omitempty"`
	// ReducerInstructions is the optional reducer prompt text; non-empty text
	// requires a reducer and NUL text makes Instructions invalid.
	ReducerInstructions string `json:"reducer_instructions,omitempty"`
}

// TurnInstruction binds prompt text to one participant turn. ParticipantTurn
// is one-based and invalid outside the plan schedule or when Actor is not a
// participant.
type TurnInstruction struct {
	// ParticipantTurn is the one-based schedule turn; a non-positive or
	// out-of-range value, or a duplicate turn, makes the instruction invalid.
	ParticipantTurn int `json:"participant_turn"`
	// Actor names the participant receiving the instruction; an unknown or
	// control actor makes the instruction invalid.
	Actor string `json:"actor"`
	// Instructions is the non-empty prompt text without NUL; empty or NUL-bearing
	// text makes the instruction invalid.
	Instructions string `json:"instructions"`
}

// Lifecycle records normalized resume, steering, and dynamic child policy. It
// is invalid when any field is not LifecycleAllow or LifecycleForbid, or when
// its combinations contradict ChildPolicy.
type Lifecycle struct {
	// Resume controls whether the session may resume; a value other than
	// LifecycleAllow or LifecycleForbid makes Lifecycle invalid.
	Resume string `json:"resume"`
	// Steering controls whether a resume may carry steering text; a value other
	// than LifecycleAllow or LifecycleForbid makes Lifecycle invalid.
	Steering string `json:"steering"`
	// Dynamic controls whether dynamic children may be created; a value other than
	// LifecycleAllow or LifecycleForbid makes Lifecycle invalid.
	Dynamic string `json:"dynamic"`
}

// ParticipantActorForTurn returns the participant actor selected by the
// schedule for a one-based turn. It supports turns beyond the configured
// schedule so the engine can apply the same selection rule to explicitly
// granted resume turns; callers validating an immutable plan should enforce
// the plan's configured turn range separately.
func ParticipantActorForTurn(value Plan, turn int) (string, error) {
	if turn < 1 {
		return "", fmt.Errorf("participant turn %d must be positive", turn)
	}
	controls := make(map[string]bool, 2)
	if value.Facilitator != nil {
		controls[value.Facilitator.Actor] = true
	}
	if value.Reducer != nil {
		controls[value.Reducer.Actor] = true
	}
	participants := make([]string, 0, len(value.Actors))
	for _, actor := range value.Actors {
		if !controls[actor.ID] {
			participants = append(participants, actor.ID)
		}
	}
	switch value.Schedule.Kind {
	case ScheduleDialogue:
		if len(participants) == 0 {
			return "", errors.New("dialogue schedule has no participant actors")
		}
		return participants[(turn-1)%len(participants)], nil
	case ScheduleSequence:
		if len(value.Schedule.Order) == 0 {
			return "", errors.New("sequence schedule has no participant order")
		}
		return value.Schedule.Order[(turn-1)%len(value.Schedule.Order)], nil
	default:
		return "", fmt.Errorf("schedule kind %q is not supported", value.Schedule.Kind)
	}
}

// Validate checks the complete portable plan and returns an error naming the
// invalid field or relationship. It never applies defaults or panics; any
// failed required, enum, relationship, JSON, blob, or portability check makes
// the plan invalid.
func Validate(value Plan) error {
	if value.Kind != PlanKind {
		return fmt.Errorf("plan kind %q does not match build value %q", value.Kind, PlanKind)
	}
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("plan schema_version %d does not match build value %d", value.SchemaVersion, SchemaVersion)
	}
	switch value.Provenance {
	case ProvenanceOrdinary, ProvenanceRecipe, ProvenanceChild, ProvenanceSupplied:
	default:
		return fmt.Errorf("plan provenance must be one of %s, %s, %s, %s", ProvenanceOrdinary, ProvenanceRecipe, ProvenanceChild, ProvenanceSupplied)
	}
	if (value.Provenance == ProvenanceRecipe || value.Provenance == ProvenanceChild) && value.RecipeID == "" {
		return errors.New("plan compiled from a recipe must record recipe_id")
	}
	if strings.TrimSpace(value.Task) == "" {
		return errors.New("plan must state a task")
	}
	if value.Timeouts.TurnSeconds <= 0 {
		return errors.New("plan timeouts.turn_seconds must be positive")
	}
	if value.Timeouts.StallSeconds <= 0 {
		return errors.New("plan timeouts.stall_seconds must be positive")
	}
	switch value.Mode {
	case ModeAdversarial, ModeCooperative, ModeSteelman:
	default:
		return fmt.Errorf("plan mode must be one of %s, %s, %s", ModeAdversarial, ModeCooperative, ModeSteelman)
	}
	switch value.Investigation {
	case InvestigationAuto, InvestigationNormal, InvestigationContextOnly:
	default:
		return fmt.Errorf("plan investigation must be one of %s, %s, %s", InvestigationAuto, InvestigationNormal, InvestigationContextOnly)
	}
	if err := validateToken("session_id", value.SessionID); err != nil {
		return err
	}
	if len(value.Actors) == 0 {
		return errors.New("plan must contain at least one actor")
	}
	actorIDs := make(map[string]bool, len(value.Actors))
	childActors := make(map[string]bool, len(value.Actors))
	for _, actor := range value.Actors {
		if err := validateToken("actor.id", actor.ID); err != nil {
			return err
		}
		if _, exists := actorIDs[actor.ID]; exists {
			return fmt.Errorf("plan contains duplicate actor %q", actor.ID)
		}
		actorIDs[actor.ID] = false
		switch actor.Backend {
		case ActorBackendClaude, ActorBackendCodex, ActorBackendGemini:
			if actor.ChildRecipeID != "" || actor.ChildTurns != 0 {
				return fmt.Errorf("provider actor %q must not declare a child step", actor.ID)
			}
		case ActorBackendChild:
			if err := validateToken("actor.child_recipe_id", actor.ChildRecipeID); err != nil {
				return err
			}
			if actor.ChildTurns < 0 {
				return errors.New("actor.child_turns must not be negative")
			}
			childActors[actor.ID] = true
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
	if value.Schedule.Turns < 1 {
		return errors.New("schedule turns must be positive")
	}
	if value.Facilitator != nil {
		if _, exists := actorIDs[value.Facilitator.Actor]; !exists {
			return errors.New("facilitator actor is not in actors")
		}
		if childActors[value.Facilitator.Actor] {
			return errors.New("facilitator actor must not be a child step")
		}
		if value.Facilitator.Cadence < 1 {
			return errors.New("facilitator cadence must be positive")
		}
	}
	if value.Reducer != nil {
		if _, exists := actorIDs[value.Reducer.Actor]; !exists {
			return errors.New("reducer actor is not in actors")
		}
		if childActors[value.Reducer.Actor] {
			return errors.New("reducer actor must not be a child step")
		}
	}
	controlActorCount := 0
	if value.Facilitator != nil {
		controlActorCount++
	}
	if value.Reducer != nil && (value.Facilitator == nil || value.Reducer.Actor != value.Facilitator.Actor) {
		controlActorCount++
	}
	switch value.Schedule.Kind {
	case ScheduleDialogue:
		if len(value.Schedule.Order) != 0 {
			return errors.New("schedule.order is only valid for a sequence schedule")
		}
		if participants := len(value.Actors) - controlActorCount; participants != 2 {
			return fmt.Errorf("dialogue schedule requires exactly two participants, got %d", participants)
		}
	case ScheduleSequence:
		if len(value.Schedule.Order) != value.Schedule.Turns {
			return fmt.Errorf("sequence schedule order must contain exactly %d entries", value.Schedule.Turns)
		}
		scheduledActorCount := 0
		for _, actorID := range value.Schedule.Order {
			scheduled, exists := actorIDs[actorID]
			if !exists {
				return fmt.Errorf("schedule.order names unknown actor %q", actorID)
			}
			if (value.Facilitator != nil && value.Facilitator.Actor == actorID) || (value.Reducer != nil && value.Reducer.Actor == actorID) {
				return fmt.Errorf("schedule.order must not name control actor %q", actorID)
			}
			if !scheduled {
				actorIDs[actorID] = true
				scheduledActorCount++
			}
		}
		if scheduledActorCount != len(value.Actors)-controlActorCount {
			return errors.New("sequence schedule must include every actor that does not hold a control role")
		}
	default:
		return errors.New("schedule kind must be dialogue or sequence")
	}
	if value.ProviderRetry.Mode != ProviderRetryAllow && value.ProviderRetry.Mode != ProviderRetryForbid {
		return errors.New("provider_retry mode must be allow or forbid")
	}
	if value.ProviderRetry.MaxAttempts < 1 {
		return errors.New("provider_retry max_attempts must be positive")
	}
	if value.ProviderRetry.Mode == ProviderRetryForbid && value.ProviderRetry.MaxAttempts != 1 {
		return errors.New("provider_retry forbid mode requires exactly one attempt")
	}
	if value.Workspace.Mode != WorkspaceModeCurrent && value.Workspace.Mode != WorkspaceModeHeadCopy {
		return errors.New("workspace mode must be current or head-copy")
	}
	for _, group := range []struct {
		label  string
		inputs []Input
	}{
		{label: "input", inputs: value.Inputs},
		{label: "context", inputs: value.Context},
		{label: "skill", inputs: value.Skills},
	} {
		if err := validateInputs(group.label, group.inputs); err != nil {
			return err
		}
	}
	if len(value.TaskPlan) > 0 {
		if _, err := semanticjson.SemanticJSONBytesRaw(value.TaskPlan); err != nil {
			return fmt.Errorf("task_plan is not strict JSON: %w", err)
		}
	}
	switch value.ChildPolicy.Mode {
	case ChildPolicyDeny, ChildPolicyAsk, ChildPolicyAllow:
	default:
		return errors.New("child_policy mode must be deny, ask, or allow")
	}
	if value.ChildPolicy.MaxDepth < 0 || value.ChildPolicy.MaxChildren < 0 || value.ChildPolicy.MaxTurns < 0 {
		return errors.New("child policy limits must not be negative")
	}
	if err := validateUniqueTokens("child_policy.allowed_recipes", value.ChildPolicy.AllowedRecipes); err != nil {
		return err
	}
	switch value.Result.Source {
	case ResultSourceLastTurn, ResultSourceReducer:
	default:
		return errors.New("result source must be last_turn or reducer")
	}
	if value.Result.Source == ResultSourceReducer && value.Reducer == nil {
		return errors.New("reducer result source requires a reducer")
	}
	if value.Result.Format != ResultFormatText && value.Result.Format != ResultFormatJSON {
		return errors.New("result format must be text or json")
	}
	if len(value.Result.Schema) > 0 {
		if _, err := semanticjson.SemanticJSONBytesRaw(value.Result.Schema); err != nil {
			return fmt.Errorf("result schema is not strict JSON: %w", err)
		}
	}
	if err := validateLifecycle(value.Lifecycle); err != nil {
		return err
	}
	if value.Lifecycle != nil && value.Lifecycle.Dynamic == LifecycleForbid && value.ChildPolicy.Mode != ChildPolicyDeny {
		return errors.New("lifecycle dynamic forbid requires child_policy mode deny")
	}
	if value.Lifecycle != nil && value.Lifecycle.Resume == LifecycleForbid && value.ChildPolicy.Mode == ChildPolicyAsk {
		return errors.New("lifecycle.resume forbid is incompatible with child_policy.mode ask")
	}
	if err := validateInstructions(value, actorIDs); err != nil {
		return err
	}
	return semanticjson.ValidatePortableValue(portableProjection(value))
}

// Digest validates value and returns its semantic-JSON digest. It returns an
// error instead of a digest when the plan is invalid or cannot be encoded.
func Digest(value Plan) (string, error) {
	if err := Validate(value); err != nil {
		return "", err
	}
	return semanticjson.SemanticJSONDigest(value)
}

// CanonicalBytes validates value and returns the exact semantic-JSON bytes for
// the plan document. It returns an error when the plan is invalid or cannot be
// encoded.
func CanonicalBytes(value Plan) ([]byte, error) {
	if err := Validate(value); err != nil {
		return nil, err
	}
	return semanticjson.SemanticJSONBytes(value)
}

// Equal reports whether two valid plans have identical canonical semantic-JSON
// bytes. It returns false when either plan is invalid or when any plan field,
// including exact JSON schema values, differs.
func (value Plan) Equal(other Plan) bool {
	left, leftErr := CanonicalBytes(value)
	right, rightErr := CanonicalBytes(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// BlobRefs returns plan input, context, and skill references in document order.
// It does not validate the plan; malformed or empty groups simply contribute
// the references they contain.
func BlobRefs(value Plan) []BlobRef {
	refs := make([]BlobRef, 0, len(value.Inputs)+len(value.Context)+len(value.Skills))
	for _, inputs := range [][]Input{value.Inputs, value.Context, value.Skills} {
		for _, input := range inputs {
			refs = append(refs, input.Contents...)
		}
	}
	return refs
}

func validateInstructions(value Plan, actors map[string]bool) error {
	instructions := value.Instructions
	if instructions == nil {
		return nil
	}
	turns := make(map[int]struct{}, len(instructions.Turns))
	for _, turn := range instructions.Turns {
		if turn.ParticipantTurn < 1 {
			return errors.New("instruction participant_turn must be positive")
		}
		if turn.ParticipantTurn > value.Schedule.Turns {
			return fmt.Errorf("instruction participant_turn %d is outside the schedule", turn.ParticipantTurn)
		}
		if _, exists := turns[turn.ParticipantTurn]; exists {
			return fmt.Errorf("plan contains duplicate instruction for participant turn %d", turn.ParticipantTurn)
		}
		turns[turn.ParticipantTurn] = struct{}{}
		if _, exists := actors[turn.Actor]; !exists {
			return fmt.Errorf("instruction names unknown actor %q", turn.Actor)
		}
		if (value.Facilitator != nil && turn.Actor == value.Facilitator.Actor) ||
			(value.Reducer != nil && turn.Actor == value.Reducer.Actor) {
			return fmt.Errorf("instruction must name a participant actor, got %q", turn.Actor)
		}
		scheduledActor, err := ParticipantActorForTurn(value, turn.ParticipantTurn)
		if err != nil {
			return fmt.Errorf("select actor for instruction participant turn %d: %w", turn.ParticipantTurn, err)
		}
		if turn.Actor != scheduledActor {
			return fmt.Errorf("instruction for participant turn %d names actor %q, but schedule selects actor %q", turn.ParticipantTurn, turn.Actor, scheduledActor)
		}
		if strings.TrimSpace(turn.Instructions) == "" || strings.Contains(turn.Instructions, "\x00") {
			return fmt.Errorf("instruction for participant turn %d is invalid", turn.ParticipantTurn)
		}
	}
	if instructions.ReducerInstructions != "" {
		if value.Reducer == nil {
			return errors.New("reducer instructions require a reducer")
		}
		if strings.Contains(instructions.ReducerInstructions, "\x00") {
			return errors.New("reducer instructions contain a control character")
		}
	}
	if value.Result.Source == ResultSourceReducer && strings.TrimSpace(instructions.ReducerInstructions) == "" {
		return errors.New("reducer result requires reducer instructions")
	}
	return nil
}

func validateToken(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a control character", label)
	}
	return nil
}

func validateOptionalToken(label, value string) error {
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
		if len(input.Contents) == 0 {
			return fmt.Errorf("%s %q must contain at least one payload", label, input.Name)
		}
		for _, content := range input.Contents {
			if err := validateBlobRef(content); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBlobRef(value BlobRef) error {
	if !validSHA256(value.SHA256) {
		return errors.New("invalid blob ref: sha256 must be 64 lower-case hexadecimal characters")
	}
	if value.Size < 0 {
		return errors.New("invalid blob ref: size must not be negative")
	}
	if strings.TrimSpace(value.MediaType) == "" {
		return errors.New("invalid blob ref: media_type is required")
	}
	if strings.ContainsAny(value.MediaType, "\r\n\x00") {
		return errors.New("invalid blob ref: media_type contains a control character")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
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
		if field.value != LifecycleAllow && field.value != LifecycleForbid {
			return fmt.Errorf("%s must be allow or forbid", field.label)
		}
	}
	return nil
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

func portableProjection(value Plan) Plan {
	value.Task = ""
	return value
}
