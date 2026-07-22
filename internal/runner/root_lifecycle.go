package runner

import (
	"errors"
	"os"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
)

const (
	diagnosticCodeRootLifecycleForbidden = "root_recipe_lifecycle_forbidden"
	diagnosticCodeRootLifecycleInvalid   = "root_recipe_lifecycle_invalid"
)

const (
	rootLifecycleActionResume             = "resume"
	rootLifecycleActionSteer              = "steer"
	rootLifecycleActionProposalCreate     = "proposal_create"
	rootLifecycleActionProposalApprove    = "proposal_approve"
	rootLifecycleActionProposalReject     = "proposal_reject"
	rootLifecycleActionAutomaticExpansion = "automatic_expansion"
)

// guardRootLifecycleSession is deliberately read-only. Mutation entry points
// call it before acquiring the session mutation lock so a forbidden root
// action cannot create or touch lock state on its rejection path.
func guardRootLifecycleSession(sessionDir string, action string) error {
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		// Some low-level proposal helpers are also used before a session exists.
		// With no root-session metadata there is no lifecycle policy to enforce.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return guardRootLifecycleMeta(meta, action)
}

func guardRootLifecycleMap(meta map[string]any, action string) error {
	return guardRootLifecycleMeta(model.NewSessionMeta(meta), action)
}

func guardRootLifecycleMeta(meta model.SessionMeta, action string) error {
	if meta.String("execution_kind") != "recipe" {
		return nil
	}
	field, ok := rootLifecycleField(action)
	if !ok {
		return rootRecipeDiagnostic(
			diagnosticCodeRootLifecycleInvalid,
			contracts.DiagnosticPhasePolicy,
			"/lifecycle",
			"Root recipe lifecycle action is not recognized.",
			map[string]any{"action": action},
		)
	}
	lifecycle, ok := meta.Get("lifecycle").(map[string]any)
	if !ok || lifecycle == nil {
		return rootRecipeDiagnostic(
			diagnosticCodeRootLifecycleInvalid,
			contracts.DiagnosticPhasePolicy,
			"/lifecycle",
			"Root recipe lifecycle metadata is missing or invalid.",
			map[string]any{"action": action},
		)
	}
	policy := strings.TrimSpace(stringFromAny(lifecycle[field]))
	switch policy {
	case "allow":
		return nil
	case "forbid":
		return rootRecipeDiagnostic(
			diagnosticCodeRootLifecycleForbidden,
			contracts.DiagnosticPhasePolicy,
			"/lifecycle/"+field,
			"The root recipe lifecycle policy forbids this operation.",
			map[string]any{"action": action, "policy": field, "value": policy},
		)
	default:
		return rootRecipeDiagnostic(
			diagnosticCodeRootLifecycleInvalid,
			contracts.DiagnosticPhasePolicy,
			"/lifecycle/"+field,
			"Root recipe lifecycle policy must be allow or forbid.",
			map[string]any{"action": action, "policy": field, "value": emptyStringAsNil(policy)},
		)
	}
}

func rootLifecycleField(action string) (string, bool) {
	switch action {
	case rootLifecycleActionResume:
		return "resume", true
	case rootLifecycleActionSteer:
		return "steering", true
	case rootLifecycleActionProposalCreate,
		rootLifecycleActionProposalApprove,
		rootLifecycleActionProposalReject,
		rootLifecycleActionAutomaticExpansion:
		return "dynamic", true
	default:
		return "", false
	}
}
