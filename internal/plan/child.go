package plan

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/session"
)

// ForChild derives a child plan from a validated parent. It does not select an
// execution path from parent provenance or kind: it carries the same typed
// plan data forward with bounded budget and child provenance for inspection.
func ForChild(parent session.Plan, request ChildRequest) (session.Plan, error) {
	if err := session.ValidatePlan(parent); err != nil {
		return session.Plan{}, fmt.Errorf("validate parent plan: %w", err)
	}
	recipeID := strings.TrimSpace(request.RecipeID)
	if recipeID == "" {
		return session.Plan{}, fmt.Errorf("child recipe id is required")
	}
	if request.Turns < 0 {
		return session.Plan{}, fmt.Errorf("child turns must not be negative")
	}
	if err := childRequestAllowed(parent.ChildPolicy, recipeID); err != nil {
		return session.Plan{}, err
	}

	turns, err := childTurns(parent.Schedule.Turns, parent.ChildPolicy.MaxTurns, request.Turns)
	if err != nil {
		return session.Plan{}, err
	}
	participants, err := participantsFromParent(parent)
	if err != nil {
		return session.Plan{}, err
	}
	schedule := copySchedule(parent.Schedule)
	schedule.Turns = turns
	if schedule.Kind == "sequence" {
		if len(schedule.Order) < turns {
			return session.Plan{}, fmt.Errorf("parent sequence order has %d actors for child budget %d", len(schedule.Order), turns)
		}
		schedule.Order = append([]string{}, schedule.Order[:turns]...)
	}

	childPolicy := copyChildPolicy(parent.ChildPolicy)
	childPolicy.MaxDepth--
	childPolicy.MaxChildren--
	childPolicy.MaxTurns--
	childSessionID := strings.TrimSpace(request.SessionID)
	if childSessionID == "" {
		childSessionID = parent.SessionID + "-child"
	}
	return compile(planSpec{
		sessionID:     childSessionID,
		provenance:    session.ProvenanceChild,
		recipeID:      recipeID,
		task:          request.Question,
		timeouts:      parent.Timeouts,
		mode:          parent.Mode,
		investigation: parent.Investigation,
		actors:        parent.Actors,
		participants:  participants,
		schedule:      schedule,
		facilitator:   parent.Facilitator,
		reducer:       parent.Reducer,
		retry:         parent.ProviderRetry,
		workspace:     parent.Workspace,
		inputs:        parent.Inputs,
		childPolicy:   childPolicy,
		result:        parent.Result,
	})
}

func childRequestAllowed(policy session.ChildPolicy, recipeID string) error {
	switch policy.Mode {
	case "deny":
		return fmt.Errorf("child policy denies child plans")
	case "ask", "allow":
	default:
		return fmt.Errorf("child policy mode %q is not supported", policy.Mode)
	}
	if policy.MaxDepth <= 0 {
		return fmt.Errorf("child policy has no remaining depth")
	}
	if policy.MaxChildren <= 0 {
		return fmt.Errorf("child policy has no remaining child capacity")
	}
	if policy.MaxTurns <= 0 {
		return fmt.Errorf("child policy has no remaining turn budget")
	}
	if len(policy.AllowedRecipes) == 0 {
		return nil
	}
	for _, allowed := range policy.AllowedRecipes {
		if allowed == recipeID {
			return nil
		}
	}
	return fmt.Errorf("child recipe %q is not allowed by parent policy", recipeID)
}

func childTurns(parentTurns int, maxTurns int, requestedTurns int) (int, error) {
	if parentTurns <= 0 {
		return 0, fmt.Errorf("parent schedule has no turn budget")
	}
	if maxTurns <= 0 {
		return 0, fmt.Errorf("child policy has no remaining turn budget")
	}
	budget := maxTurns
	if parentTurns < budget {
		budget = parentTurns
	}
	if requestedTurns == 0 || requestedTurns > budget {
		return budget, nil
	}
	return requestedTurns, nil
}

func copyChildPolicy(value session.ChildPolicy) session.ChildPolicy {
	value.AllowedRecipes = append([]string{}, value.AllowedRecipes...)
	if value.AllowedRecipes == nil {
		value.AllowedRecipes = []string{}
	}
	return value
}

func participantsFromParent(parent session.Plan) ([]string, error) {
	roleActors := make(map[string]struct{}, 2)
	if parent.Facilitator != nil {
		roleActors[parent.Facilitator.Actor] = struct{}{}
	}
	if parent.Reducer != nil {
		roleActors[parent.Reducer.Actor] = struct{}{}
	}
	participants := make([]string, 0, len(parent.Actors))
	for _, actor := range parent.Actors {
		if _, role := roleActors[actor.ID]; !role {
			participants = append(participants, actor.ID)
		}
	}
	if len(participants) == 0 {
		return nil, fmt.Errorf("parent plan has no scheduled participants outside control roles")
	}
	return participants, nil
}
