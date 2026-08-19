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

	first := flagActor("slot_0", agents[0], flags.ModelA, flags.EffortA)
	second := flagActor("slot_1", agents[1], flags.ModelB, flags.EffortB)
	facilitatorBackend := strings.TrimSpace(flags.FacilitatorBackend)
	if facilitatorBackend == "" {
		if shorthand {
			facilitatorBackend = agents[0]
		} else {
			facilitatorBackend = "codex"
		}
	}
	facilitator := flagActor("facilitator", facilitatorBackend, flags.FacilitatorModel, flags.FacilitatorEffort)

	policy, err := policyFromDynamic(flags.ChildPolicy, flags.Dynamic)
	if err != nil {
		return session.Plan{}, err
	}
	return compile(session.Plan{
		Provenance:    session.ProvenanceOrdinary,
		SessionID:     flags.SessionID,
		Task:          flags.Task,
		Timeouts:      session.Timeouts{TurnSeconds: flags.TimeoutSeconds, StallSeconds: flags.StallTimeoutSeconds},
		Mode:          flags.Mode,
		Investigation: flags.Investigation,
		Actors:        []session.Actor{first, second, facilitator},
		Schedule: session.Schedule{
			Kind:              "dialogue",
			Turns:             turns,
			StopOnConvergence: stopOnConvergence,
		},
		Facilitator: &session.Facilitator{Actor: "facilitator", Cadence: 1},
		Workspace:   flags.Workspace,
		Inputs:      flags.Inputs,
		Context:     flags.Context,
		Skills:      flags.Skills,
		TaskPlan:    flags.TaskPlan,
		ChildPolicy: policy,
		Result:      flags.Result,
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

func flagActor(id string, backend string, model string, effort string) session.Actor {
	return session.Actor{
		ID:      id,
		Backend: strings.TrimSpace(backend),
		Model:   strings.TrimSpace(model),
		Effort:  strings.TrimSpace(effort),
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
