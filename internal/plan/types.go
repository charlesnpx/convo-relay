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
	ChildPolicy         session.ChildPolicy
	Result              session.Result
}

// Catalog is the typed collection used to resolve a named recipe. Inline
// recipes bypass it; neither route exposes an untyped configuration value.
type Catalog struct {
	Recipes []Recipe `json:"recipes"`
}

// Recipe is a fully resolved recipe. Actors includes participants and any
// distinct facilitator or reducer actor. Participants identifies which actor
// ids belong to the schedule. Role actors must be separate from scheduled
// participants, even when they use the same backend, model, and effort.
type Recipe struct {
	ID            string               `json:"id"`
	Actors        []session.Actor      `json:"actors"`
	Participants  []string             `json:"participants"`
	Schedule      session.Schedule     `json:"schedule"`
	Mode          string               `json:"mode"`
	Investigation string               `json:"investigation"`
	Facilitator   *session.Facilitator `json:"facilitator,omitempty"`
	Reducer       *session.Reducer     `json:"reducer,omitempty"`

	ProviderRetry session.ProviderRetry `json:"provider_retry"`
	Workspace     session.Workspace     `json:"workspace"`
	Inputs        []session.Input       `json:"inputs"`
	ChildPolicy   session.ChildPolicy   `json:"child_policy"`
	Result        session.Result        `json:"result"`
}

// RecipeInput selects either a named recipe from Catalog or the fully typed
// Inline recipe. Timeouts are launch policy rather than recipe content, just as
// they are supplied alongside --recipe by the current CLI.
type RecipeInput struct {
	SessionID string
	Task      string
	RecipeID  string
	Inline    *Recipe
	Catalog   Catalog
	// Raw is either one Recipe JSON object or a Catalog JSON object. It is
	// decoded exactly once inside this package before compilation; callers never
	// receive an untyped decoded representation.
	Raw      json.RawMessage
	Timeouts session.Timeouts
}

// ChildRequest supplies the operator-approved child identity and question.
// Turns is optional; zero uses the remaining parent child-turn budget.
type ChildRequest struct {
	SessionID string
	RecipeID  string
	Question  string
	Turns     int
}
