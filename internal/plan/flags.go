package plan

import (
	"fmt"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/session"
)

// FromFlags compiles the ordinary run flags into a dialogue plan.
func FromFlags(flags Flags) (session.Plan, error) {
	agents, shorthand, err := parseAgents(flags.Agents)
	if err != nil {
		return session.Plan{}, err
	}
	turns, stopOnConvergence, err := flagSchedule(flags)
	if err != nil {
		return session.Plan{}, err
	}

	first, err := flagActor("slot_0", agents[0], flags.ModelA, flags.EffortA)
	if err != nil {
		return session.Plan{}, err
	}
	second, err := flagActor("slot_1", agents[1], flags.ModelB, flags.EffortB)
	if err != nil {
		return session.Plan{}, err
	}
	facilitatorBackend := strings.TrimSpace(flags.FacilitatorBackend)
	if facilitatorBackend == "" {
		if shorthand && agents[0] != "relay" {
			facilitatorBackend = agents[0]
		} else {
			facilitatorBackend = "codex"
		}
	}
	facilitator, err := flagActor("facilitator", facilitatorBackend, flags.FacilitatorModel, flags.FacilitatorEffort)
	if err != nil {
		return session.Plan{}, err
	}
	if facilitator.Backend == "relay" {
		return session.Plan{}, fmt.Errorf("facilitator backend %q is not supported", facilitator.Backend)
	}

	policy, err := policyFromDynamic(flags.ChildPolicy, flags.Dynamic)
	if err != nil {
		return session.Plan{}, err
	}
	return compile(planSpec{
		sessionID:     flags.SessionID,
		provenance:    session.ProvenanceOrdinary,
		task:          flags.Task,
		timeouts:      session.Timeouts{TurnSeconds: flags.TimeoutSeconds, StallSeconds: flags.StallTimeoutSeconds},
		mode:          flags.Mode,
		investigation: flags.Investigation,
		actors:        []session.Actor{first, second, facilitator},
		participants:  []string{"slot_0", "slot_1"},
		schedule: session.Schedule{
			Kind:              "dialogue",
			Turns:             turns,
			StopOnConvergence: stopOnConvergence,
			Order:             []string{},
		},
		facilitator: &session.Facilitator{Actor: "facilitator", Cadence: 1},
		retry:       session.ProviderRetry{},
		workspace:   flags.Workspace,
		inputs:      flags.Inputs,
		childPolicy: policy,
		result:      flags.Result,
	})
}

func parseAgents(raw string) ([]string, bool, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "codex,codex"
	}
	parts := strings.Split(raw, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
		if parts[index] == "" {
			return nil, false, fmt.Errorf("agents requires one backend or two comma-separated backends")
		}
	}
	switch len(parts) {
	case 1:
		return []string{parts[0], parts[0]}, true, nil
	case 2:
		return parts, false, nil
	default:
		return nil, false, fmt.Errorf("agents requires one backend or two comma-separated backends")
	}
}

func flagSchedule(flags Flags) (int, bool, error) {
	if flags.Rounds < 0 {
		return 0, false, fmt.Errorf("rounds must not be negative")
	}
	if flags.MaxRounds < 0 {
		return 0, false, fmt.Errorf("max rounds must not be negative")
	}
	if flags.Quick {
		return 3, false, nil
	}
	if flags.Rounds > 0 {
		return flags.Rounds, false, nil
	}
	if flags.MaxRounds == 0 {
		return defaultMaxRounds, true, nil
	}
	return flags.MaxRounds, true, nil
}

func flagActor(id string, backend string, model string, effort string) (session.Actor, error) {
	backend = strings.TrimSpace(backend)
	defaults, found := actorDefaults(backend)
	if !found {
		return session.Actor{}, fmt.Errorf("unknown backend %q", backend)
	}
	if strings.TrimSpace(model) == "" {
		model = defaults.model
	}
	if strings.TrimSpace(effort) == "" {
		effort = defaults.effort
	}
	return session.Actor{ID: id, Backend: backend, Model: model, Effort: effort}, nil
}

type backendDefaults struct {
	model  string
	effort string
}

func actorDefaults(backend string) (backendDefaults, bool) {
	switch backend {
	case "codex":
		return backendDefaults{model: "gpt-5.5", effort: "medium"}, true
	case "claude":
		return backendDefaults{model: "sonnet", effort: "medium"}, true
	case "gemini":
		return backendDefaults{model: "gemini-2.5-pro", effort: "medium"}, true
	case "relay":
		return backendDefaults{model: "review-panel", effort: "2"}, true
	default:
		return backendDefaults{}, false
	}
}

func policyFromDynamic(policy session.ChildPolicy, dynamic string) (session.ChildPolicy, error) {
	mode, err := childModeFromDynamic(dynamic)
	if err != nil {
		return session.ChildPolicy{}, err
	}
	policy = normalizeChildPolicy(policy)
	policy.Mode = mode
	return policy, nil
}

func childModeFromDynamic(dynamic string) (string, error) {
	switch strings.TrimSpace(dynamic) {
	case "", "off":
		return "deny", nil
	case "ask":
		return "ask", nil
	case "auto-safe":
		return "allow", nil
	default:
		return "", fmt.Errorf("dynamic mode must be off, ask, or auto-safe")
	}
}
