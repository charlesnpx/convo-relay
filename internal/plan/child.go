package plan

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

// ForChild resolves the requested typed recipe and compiles that recipe's
// execution behaviour under the remaining parent child budget.
func ForChild(parent session.Plan, request ChildRequest, recipes []Recipe) (session.Plan, error) {
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
	recipe, err := selectNamedRecipe(recipeID, recipes)
	if err != nil {
		return session.Plan{}, err
	}
	if err := validateRecipeProjection(recipe); err != nil {
		return session.Plan{}, err
	}
	if err := childRequestAllowed(parent.ChildPolicy, recipeID); err != nil {
		return session.Plan{}, err
	}

	childSessionID := strings.TrimSpace(request.SessionID)
	if childSessionID == "" {
		childSessionID = parent.SessionID + "-child"
	}
	child := planFromRecipe(RecipeInput{
		SessionID: childSessionID,
		Task:      request.Question,
		Timeouts:  parent.Timeouts,
		Context:   parent.Context,
		Skills:    parent.Skills,
		TaskPlan:  parent.TaskPlan,
	}, recipe, session.ProvenanceChild)
	child.Schedule = boundedChildSchedule(child.Schedule, parent, request.Turns)
	child.ChildPolicy = remainingChildPolicy(normalizeChildPolicy(child.ChildPolicy), parent.ChildPolicy)
	return compile(child)
}

// ForStaticChild compiles a child step declared by the parent recipe. Static
// composition is already admitted by that immutable plan, so it does not use
// the parent's dynamic-child approval mode or budget. Its depth bound still
// applies to any children the static child might itself request.
func ForStaticChild(parent session.Plan, request ChildRequest, recipes []Recipe) (session.Plan, error) {
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
	if parent.ChildPolicy.MaxDepth <= 0 {
		return session.Plan{}, fmt.Errorf("static child step has no remaining depth")
	}
	recipe, err := selectNamedRecipe(recipeID, recipes)
	if err != nil {
		return session.Plan{}, err
	}
	if err := validateRecipeProjection(recipe); err != nil {
		return session.Plan{}, err
	}

	childSessionID := strings.TrimSpace(request.SessionID)
	if childSessionID == "" {
		childSessionID = parent.SessionID + "-child"
	}
	child := planFromRecipe(RecipeInput{
		SessionID: childSessionID,
		Task:      request.Question,
		Timeouts:  parent.Timeouts,
		Context:   parent.Context,
		Skills:    parent.Skills,
		TaskPlan:  parent.TaskPlan,
	}, recipe, session.ProvenanceChild)
	child.Schedule = boundedStaticChildSchedule(child.Schedule, request.Turns)
	child.ChildPolicy = remainingStaticChildPolicy(normalizeChildPolicy(child.ChildPolicy), parent.ChildPolicy)
	return compile(child)
}

func childRequestAllowed(policy session.ChildPolicy, recipeID string) error {
	// ValidatePlan(parent) has already limited the mode to deny, ask or allow, so
	// only the deny rejection is reachable here.
	if policy.Mode == "deny" {
		return fmt.Errorf("child policy denies child plans")
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

func boundedChildSchedule(schedule session.Schedule, parent session.Plan, requestedTurns int) session.Schedule {
	limit := parent.Schedule.Turns
	if parent.ChildPolicy.MaxTurns < limit {
		limit = parent.ChildPolicy.MaxTurns
	}
	if requestedTurns > 0 && requestedTurns < limit {
		limit = requestedTurns
	}
	if schedule.Turns > limit {
		schedule.Turns = limit
	}
	if schedule.Kind == "sequence" && len(schedule.Order) > schedule.Turns {
		schedule.Order = schedule.Order[:schedule.Turns]
	}
	return schedule
}

func boundedStaticChildSchedule(schedule session.Schedule, requestedTurns int) session.Schedule {
	if requestedTurns > 0 && requestedTurns < schedule.Turns {
		schedule.Turns = requestedTurns
	}
	if schedule.Kind == "sequence" && len(schedule.Order) > schedule.Turns {
		schedule.Order = schedule.Order[:schedule.Turns]
	}
	return schedule
}

func remainingChildPolicy(policy session.ChildPolicy, parent session.ChildPolicy) session.ChildPolicy {
	policy.MaxDepth = minimum(policy.MaxDepth, parent.MaxDepth-1)
	policy.MaxChildren = minimum(policy.MaxChildren, parent.MaxChildren-1)
	policy.MaxTurns = minimum(policy.MaxTurns, parent.MaxTurns-1)
	return policy
}

// A static child is declared by the immutable parent recipe, so it neither
// consumes nor inherits the parent's dynamic-child capacity. Its descendants
// still have one less available nesting level.
func remainingStaticChildPolicy(policy session.ChildPolicy, parent session.ChildPolicy) session.ChildPolicy {
	policy.MaxDepth = minimum(policy.MaxDepth, parent.MaxDepth-1)
	return policy
}

func minimum(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

// ForResume accepts only prompt material. Structural variation creates a new
// launch plan instead of being smuggled into a resume.
func ForResume(parent session.Plan, input ResumeInput) (ResumeInput, error) {
	if err := session.ValidatePlan(parent); err != nil {
		return ResumeInput{}, fmt.Errorf("validate parent plan: %w", err)
	}
	return ResumeInput{Prompt: input.Prompt}, nil
}
