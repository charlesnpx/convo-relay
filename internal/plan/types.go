// Package plan compiles supported launch inputs into the single immutable
// session.Plan execution document.
package plan

import (
	"encoding/json"

	"github.com/charlesnpx/convo-relay/internal/session"
)

// Flags is the typed subset of the ordinary run flags that changes execution
// planning. It intentionally contains values, not flag.FlagSet or raw argv, so
// callers decode command-line syntax at the edge and this package remains pure.
type Flags struct {
	SessionID     string
	Task          string
	Mode          string
	Investigation string
	Agents        string
	Rounds        int
	MaxRounds     int
	Quick         bool

	ModelA  string
	EffortA string
	ModelB  string
	EffortB string

	FacilitatorBackend string
	FacilitatorModel   string
	FacilitatorEffort  string

	TimeoutSeconds      int
	StallTimeoutSeconds int
	Dynamic             string
	Workspace           session.Workspace
	Inputs              []session.Input
	Context             []session.Input
	Skills              []session.Input
	TaskPlan            json.RawMessage
	ChildPolicy         session.ChildPolicy
	Result              session.Result
}

// Recipe is the typed, resolved projection of a canonical normalized recipe.
// Actor/profile resolution happens before this boundary; the plan compiler
// never parses raw recipe JSON or carries profile references.
type Recipe struct {
	Kind          string `json:"kind"`
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Purpose       string `json:"purpose"`

	Actors        []session.Actor      `json:"actors"`
	Schedule      session.Schedule     `json:"schedule"`
	Mode          string               `json:"mode"`
	Investigation string               `json:"investigation,omitempty"`
	Facilitator   *session.Facilitator `json:"facilitator,omitempty"`
	Reducer       *session.Reducer     `json:"reducer,omitempty"`

	MaxRounds           int                   `json:"max_rounds"`
	ParticipantTurns    int                   `json:"participant_turns"`
	ResultSource        string                `json:"result_source"`
	ProviderRetry       session.ProviderRetry `json:"provider_retry"`
	IntegrationContract string                `json:"integration_contract,omitempty"`
	MaxDepth            int                   `json:"max_depth"`
	// RequiredCapabilities remains at the recipe parse boundary: it does not
	// affect execution; no reader; not carried into session.Plan.
	RequiredCapabilities []string          `json:"required_capabilities"`
	AutoApproval         string            `json:"auto_approval"`
	MatchKeywords        []string          `json:"match_keywords"`
	Lifecycle            session.Lifecycle `json:"lifecycle"`

	Workspace   session.Workspace   `json:"workspace"`
	Inputs      []session.Input     `json:"inputs"`
	ChildPolicy session.ChildPolicy `json:"child_policy"`
	Result      session.Result      `json:"result"`
}

// RecipeInput selects either a named recipe from the typed normalized recipe
// list or a fully typed inline recipe. Timeouts and prompt inputs are launch
// policy rather than recipe content.
type RecipeInput struct {
	SessionID string
	Task      string
	RecipeID  string
	Inline    *Recipe
	Recipes   []Recipe
	Timeouts  session.Timeouts
	Context   []session.Input
	Skills    []session.Input
	TaskPlan  json.RawMessage
}

// ChildRequest supplies the operator-approved child identity and question.
// Turns is optional; zero uses the remaining parent child-turn budget.
type ChildRequest struct {
	SessionID string
	RecipeID  string
	Question  string
	Turns     int
}

// ResumeInput carries prompt material plus an optional, explicit turn-budget
// extension. Structural overrides still create a new launch plan instead of
// changing an existing session's execution shape.
type ResumeInput struct {
	Prompt     string
	Context    []session.Input
	Skills     []session.Input
	ExtraTurns int
}
