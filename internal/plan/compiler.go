package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/session"
)

const (
	defaultSessionID           = "pending"
	defaultTimeoutSeconds      = 600
	defaultStallTimeoutSeconds = 300
	defaultMaxRounds           = 50
	defaultChildDepth          = 1
	defaultChildCount          = 2
	defaultChildTurns          = 8
	defaultRetryAttempts       = 7
)

type planSpec struct {
	sessionID     string
	provenance    string
	recipeID      string
	task          string
	timeouts      session.Timeouts
	mode          string
	investigation string
	actors        []session.Actor
	participants  []string
	schedule      session.Schedule
	facilitator   *session.Facilitator
	reducer       *session.Reducer
	retry         session.ProviderRetry
	workspace     session.Workspace
	inputs        []session.Input
	childPolicy   session.ChildPolicy
	result        session.Result
}

// compile is the only constructor for the target document. Input-specific
// functions convert their own syntax to planSpec, then all plans receive the
// same defaults, semantic checks, and session-owned structural validation.
func compile(spec planSpec) (session.Plan, error) {
	timeouts, err := normalizeTimeouts(spec.timeouts)
	if err != nil {
		return session.Plan{}, err
	}
	retry, err := normalizeRetry(spec.retry)
	if err != nil {
		return session.Plan{}, err
	}
	mode := strings.TrimSpace(spec.mode)
	if mode == "" {
		mode = session.ModeAdversarial
	}
	investigation := strings.TrimSpace(spec.investigation)
	if investigation == "" {
		investigation = session.InvestigationAuto
	}

	plan := session.Plan{
		Kind:          session.PlanKind,
		SchemaVersion: session.SchemaVersion,
		SessionID:     normalizedSessionID(spec.sessionID),
		Provenance:    spec.provenance,
		RecipeID:      strings.TrimSpace(spec.recipeID),
		Task:          spec.task,
		Timeouts:      timeouts,
		Mode:          mode,
		Investigation: investigation,
		Actors:        copyActors(spec.actors),
		Schedule:      copySchedule(spec.schedule),
		Facilitator:   copyFacilitator(spec.facilitator),
		Reducer:       copyReducer(spec.reducer),
		ProviderRetry: retry,
		Workspace:     normalizeWorkspace(spec.workspace),
		Inputs:        copyInputs(spec.inputs),
		ChildPolicy:   normalizeChildPolicy(spec.childPolicy),
		Result:        normalizeResult(spec.result),
	}
	if err := session.ValidatePlan(plan); err != nil {
		return session.Plan{}, fmt.Errorf("validate compiled plan: %w", err)
	}
	if err := validateCompilerSemantics(plan, spec.participants); err != nil {
		return session.Plan{}, err
	}
	return plan, nil
}

func validateCompilerSemantics(plan session.Plan, participants []string) error {
	actorIDs := make(map[string]struct{}, len(plan.Actors))
	for _, actor := range plan.Actors {
		if !knownBackend(actor.Backend) {
			return fmt.Errorf("unknown backend %q for actor %q", actor.Backend, actor.ID)
		}
		actorIDs[actor.ID] = struct{}{}
	}

	participantIDs := make(map[string]struct{}, len(participants))
	for _, participant := range participants {
		if _, exists := actorIDs[participant]; !exists {
			return fmt.Errorf("schedule participant %q is not in actors", participant)
		}
		if _, duplicate := participantIDs[participant]; duplicate {
			return fmt.Errorf("schedule contains duplicate participant %q", participant)
		}
		participantIDs[participant] = struct{}{}
	}

	switch plan.Schedule.Kind {
	case "dialogue":
		if len(participants) != 2 {
			return fmt.Errorf("dialogue schedule requires exactly two participants, got %d", len(participants))
		}
	case "sequence":
		if len(participants) == 0 {
			return fmt.Errorf("sequence schedule requires at least one participant")
		}
	}

	roleActors := make(map[string]struct{}, 2)
	for _, role := range []struct {
		name  string
		actor string
	}{
		{name: "facilitator", actor: facilitatorActor(plan.Facilitator)},
		{name: "reducer", actor: reducerActor(plan.Reducer)},
	} {
		if role.actor == "" {
			continue
		}
		actor, found := findActor(plan.Actors, role.actor)
		if !found {
			return fmt.Errorf("validated plan is missing %s actor %q", role.name, role.actor)
		}
		if _, scheduled := participantIDs[role.actor]; scheduled {
			return fmt.Errorf("%s actor %q must be separate from scheduled participants", role.name, role.actor)
		}
		if actor.Backend == "relay" {
			return fmt.Errorf("%s actor %q cannot use relay backend", role.name, role.actor)
		}
		roleActors[role.actor] = struct{}{}
	}
	for _, actor := range plan.Actors {
		if _, participant := participantIDs[actor.ID]; participant {
			continue
		}
		if _, role := roleActors[actor.ID]; role {
			continue
		}
		return fmt.Errorf("actor %q is neither scheduled nor assigned a control role", actor.ID)
	}
	return nil
}

func normalizeTimeouts(value session.Timeouts) (session.Timeouts, error) {
	if value.TurnSeconds < 0 {
		return session.Timeouts{}, fmt.Errorf("timeout seconds must not be negative")
	}
	if value.StallSeconds < 0 {
		return session.Timeouts{}, fmt.Errorf("stall timeout seconds must not be negative")
	}
	if value.TurnSeconds == 0 {
		value.TurnSeconds = defaultTimeoutSeconds
	}
	if value.StallSeconds == 0 {
		value.StallSeconds = defaultStallTimeoutSeconds
	}
	return value, nil
}

func normalizeRetry(value session.ProviderRetry) (session.ProviderRetry, error) {
	value.Mode = strings.TrimSpace(value.Mode)
	if value.Mode == "" {
		value.Mode = "allow"
	}
	if value.Mode != "allow" && value.Mode != "forbid" {
		return session.ProviderRetry{}, fmt.Errorf("provider retry mode must be allow or forbid")
	}
	if value.MaxAttempts < 0 {
		return session.ProviderRetry{}, fmt.Errorf("provider retry max attempts must not be negative")
	}
	if value.MaxAttempts == 0 {
		if value.Mode == "forbid" {
			value.MaxAttempts = 1
		} else {
			value.MaxAttempts = defaultRetryAttempts
		}
	}
	return value, nil
}

func normalizeWorkspace(value session.Workspace) session.Workspace {
	if strings.TrimSpace(value.Mode) == "" {
		value.Mode = "current"
	}
	return value
}

func normalizeChildPolicy(value session.ChildPolicy) session.ChildPolicy {
	if strings.TrimSpace(value.Mode) == "" && value.MaxDepth == 0 && value.MaxChildren == 0 && value.MaxTurns == 0 && len(value.AllowedRecipes) == 0 {
		value = session.ChildPolicy{
			Mode:           "deny",
			MaxDepth:       defaultChildDepth,
			MaxChildren:    defaultChildCount,
			MaxTurns:       defaultChildTurns,
			AllowedRecipes: []string{},
		}
	}
	if strings.TrimSpace(value.Mode) == "" {
		value.Mode = "deny"
	}
	value.AllowedRecipes = append([]string{}, value.AllowedRecipes...)
	if value.AllowedRecipes == nil {
		value.AllowedRecipes = []string{}
	}
	return value
}

func normalizeResult(value session.Result) session.Result {
	if strings.TrimSpace(value.Format) == "" {
		value.Format = "text"
	}
	value.Schema = append(json.RawMessage{}, value.Schema...)
	return value
}

func normalizedSessionID(value string) string {
	if strings.TrimSpace(value) == "" {
		return defaultSessionID
	}
	return value
}

func copyActors(value []session.Actor) []session.Actor {
	result := append([]session.Actor{}, value...)
	if result == nil {
		return []session.Actor{}
	}
	return result
}

func copySchedule(value session.Schedule) session.Schedule {
	value.Order = append([]string{}, value.Order...)
	if value.Order == nil {
		value.Order = []string{}
	}
	return value
}

func copyFacilitator(value *session.Facilitator) *session.Facilitator {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyReducer(value *session.Reducer) *session.Reducer {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyInputs(value []session.Input) []session.Input {
	result := append([]session.Input{}, value...)
	if result == nil {
		return []session.Input{}
	}
	return result
}

func findActor(actors []session.Actor, actorID string) (session.Actor, bool) {
	for _, actor := range actors {
		if actor.ID == actorID {
			return actor, true
		}
	}
	return session.Actor{}, false
}

func facilitatorActor(value *session.Facilitator) string {
	if value == nil {
		return ""
	}
	return value.Actor
}

func reducerActor(value *session.Reducer) string {
	if value == nil {
		return ""
	}
	return value.Actor
}

func knownBackend(value string) bool {
	switch strings.TrimSpace(value) {
	case "claude", "codex", "gemini", "relay":
		return true
	default:
		return false
	}
}
