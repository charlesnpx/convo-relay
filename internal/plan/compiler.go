package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
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

// compile is the only constructor for the target document. Each input path
// builds the target shape directly; this boundary makes one defensive copy,
// applies compiler-owned defaults, and delegates structure to ValidatePlan.
func compile(plan session.Plan) (session.Plan, error) {
	plan = copyPlan(plan)
	plan.Kind = session.PlanKind
	plan.SchemaVersion = session.SchemaVersion
	plan.SessionID = normalizedSessionID(plan.SessionID)
	plan.Timeouts = normalizeTimeouts(plan.Timeouts)
	plan.Mode = normalizedMode(plan.Mode)
	plan.Investigation = normalizedInvestigation(plan.Investigation)
	plan.ProviderRetry = normalizeRetry(plan.ProviderRetry)
	plan.Workspace = normalizeWorkspace(plan.Workspace)
	plan.ChildPolicy = normalizeChildPolicy(plan.ChildPolicy)
	plan.Result = normalizeResult(plan.Result)

	if err := session.ValidatePlan(plan); err != nil {
		return session.Plan{}, fmt.Errorf("validate compiled plan: %w", err)
	}
	return plan, nil
}

func normalizeTimeouts(value session.Timeouts) session.Timeouts {
	if value.TurnSeconds == 0 {
		value.TurnSeconds = defaultTimeoutSeconds
	}
	if value.StallSeconds == 0 {
		value.StallSeconds = defaultStallTimeoutSeconds
	}
	return value
}

func normalizedMode(value string) string {
	if strings.TrimSpace(value) == "" {
		return session.ModeAdversarial
	}
	return strings.TrimSpace(value)
}

func normalizedInvestigation(value string) string {
	if strings.TrimSpace(value) == "" {
		return session.InvestigationAuto
	}
	return strings.TrimSpace(value)
}

func normalizeRetry(value session.ProviderRetry) session.ProviderRetry {
	value.Mode = strings.TrimSpace(value.Mode)
	if value.Mode == "" {
		value.Mode = "allow"
	}
	if value.MaxAttempts == 0 {
		if value.Mode == "forbid" {
			value.MaxAttempts = 1
		} else {
			value.MaxAttempts = defaultRetryAttempts
		}
	}
	return value
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
	if value.AllowedRecipes == nil {
		value.AllowedRecipes = []string{}
	}
	return value
}

func normalizeResult(value session.Result) session.Result {
	if strings.TrimSpace(value.Source) == "" {
		value.Source = "last_turn"
	}
	if strings.TrimSpace(value.Format) == "" {
		value.Format = "text"
	}
	return value
}

func normalizedSessionID(value string) string {
	if strings.TrimSpace(value) == "" {
		return defaultSessionID
	}
	return value
}

func copyPlan(plan session.Plan) session.Plan {
	plan.Actors = append([]session.Actor{}, plan.Actors...)
	if plan.Actors == nil {
		plan.Actors = []session.Actor{}
	}
	plan.Schedule.Order = append([]string{}, plan.Schedule.Order...)
	if plan.Schedule.Order == nil {
		plan.Schedule.Order = []string{}
	}
	if plan.Facilitator != nil {
		facilitator := *plan.Facilitator
		plan.Facilitator = &facilitator
	}
	if plan.Reducer != nil {
		reducer := *plan.Reducer
		plan.Reducer = &reducer
	}
	plan.Inputs = append([]session.Input{}, plan.Inputs...)
	if plan.Inputs == nil {
		plan.Inputs = []session.Input{}
	}
	copyInputContents(plan.Inputs)
	plan.Context = append([]session.Input{}, plan.Context...)
	if plan.Context == nil {
		plan.Context = []session.Input{}
	}
	copyInputContents(plan.Context)
	plan.Skills = append([]session.Input{}, plan.Skills...)
	if plan.Skills == nil {
		plan.Skills = []session.Input{}
	}
	copyInputContents(plan.Skills)
	plan.TaskPlan = append(json.RawMessage{}, plan.TaskPlan...)
	plan.ChildPolicy.AllowedRecipes = append([]string{}, plan.ChildPolicy.AllowedRecipes...)
	if plan.ChildPolicy.AllowedRecipes == nil {
		plan.ChildPolicy.AllowedRecipes = []string{}
	}
	plan.Result.Schema = append(json.RawMessage{}, plan.Result.Schema...)
	if plan.Lifecycle != nil {
		lifecycle := *plan.Lifecycle
		plan.Lifecycle = &lifecycle
	}
	return plan
}

func copyInputContents(inputs []session.Input) {
	for index := range inputs {
		inputs[index].Contents = append([]blobstore.BlobRef{}, inputs[index].Contents...)
		if inputs[index].Contents == nil {
			inputs[index].Contents = []blobstore.BlobRef{}
		}
	}
}
