package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/recipes"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type ApproveOptions struct {
	ProposalID          string
	Rounds              int
	TimeoutSeconds      int
	StallTimeoutSeconds int
	SettingsPath        string
}

type RejectOptions struct {
	ProposalID string
	Reason     string
}

type dynamicAdmittedChild struct {
	ProposalID      string
	ParentSessionID string
	ParentNodeID    string
	ChildNodeID     string
	AdmittedPlanID  string
	Task            string
	Recipe          map[string]any
	Profiles        map[string]map[string]any
	Recipes         map[string]map[string]any
	AdmittedRounds  int
	RunContext      map[string]any
	DepthPolicy     map[string]any
	SettingsPath    string
	CompiledPlan    map[string]any
	CompiledPlanRef map[string]any
}

func Proposals(sessionDir string) (map[string]any, error) {
	st := store.New(sessionDir)
	proposals, err := st.ListProposalMaps()
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(proposals))
	for _, proposal := range proposals {
		items = append(items, proposal)
	}
	return map[string]any{"session_id": st.SessionID(), "proposals": items}, nil
}

func RejectProposal(sessionDir string, opts RejectOptions) (map[string]any, error) {
	st := store.New(sessionDir)
	proposal, err := st.LoadProposalMap(opts.ProposalID)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		reason = "rejected by operator"
	}
	proposal["status"] = "rejected"
	proposal["updated_at"] = utcNow()
	proposal["rejection_reason"] = reason
	decision := makeAdmissionDecision(proposal, "reject", []string{reason}, 0, "", "", nil)
	if err := st.SaveProposalMap(proposal); err != nil {
		return nil, err
	}
	if err := st.RecordAdmissionDecisionMap(decision); err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1("spawn_rejected", stringFromAny(proposal["parent_node_id"]), "Rejected proposal "+stringFromAny(proposal["proposal_id"]), map[string]any{
		"proposal_id": proposal["proposal_id"],
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	return map[string]any{"session_id": st.SessionID(), "proposal_id": proposal["proposal_id"], "status": "rejected"}, nil
}

func ApproveProposal(ctx context.Context, sessionDir string, opts ApproveOptions) (map[string]any, error) {
	if opts.TimeoutSeconds <= 0 {
		opts.TimeoutSeconds = defaultTimeoutSeconds
	}
	if opts.StallTimeoutSeconds < 0 {
		opts.StallTimeoutSeconds = 0
	}
	lock, err := lockSessionMutation(sessionDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = lock.Unlock()
	}()
	st := store.New(sessionDir)
	meta, err := loadMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	if err := ensureSessionNotRunning(sessionDir, "approving proposals"); err != nil {
		return nil, err
	}
	transcript, err := loadTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	proposal, err := st.LoadProposalMap(opts.ProposalID)
	if err != nil {
		return nil, err
	}
	if proposal["status"] == "collapsed" {
		return map[string]any{"session_id": st.SessionID(), "proposal_id": proposal["proposal_id"], "status": "collapsed"}, nil
	}
	if proposal["status"] == "running" || proposal["status"] == "child_running" {
		return nil, fmt.Errorf("proposal %s is already %s", proposal["proposal_id"], proposal["status"])
	}
	runtimeConfig, settingsPath, err := effectiveRuntimeConfigForSession(st, meta, opts.SettingsPath)
	if err != nil {
		return nil, err
	}
	profiles, relayRecipes := runtimeConfig.BackendProfiles, runtimeConfig.RelayRecipes
	recipe := relayRecipes[stringFromAny(proposal["selected_recipe_id"])]
	if recipe == nil {
		return nil, fmt.Errorf("recipe %q is not available", proposal["selected_recipe_id"])
	}
	admittedRounds := opts.Rounds
	if admittedRounds <= 0 {
		admittedRounds = minPositiveInt(intFromAny(proposal["requested_rounds"], 1), intFromAny(recipe["max_rounds"], 1))
	}
	admitted, proposal, err := admitDynamicChild(st, meta, proposal, recipe, profiles, relayRecipes, admittedRounds, settingsPath)
	if err != nil {
		return nil, err
	}
	if envelopeRef, ok := proposal["child_result_envelope_ref"].(map[string]any); ok {
		envelope, err := st.LoadArtifact(envelopeRef)
		if err != nil {
			return nil, err
		}
		appended, err := collapseDynamicChild(st, meta, transcript, proposal, admitted, envelope, envelopeRef)
		if err != nil {
			return nil, err
		}
		status := "already_collapsed"
		if appended {
			status = "collapsed"
		}
		return map[string]any{"session_id": st.SessionID(), "proposal_id": proposal["proposal_id"], "status": status, "child_node_id": admitted.ChildNodeID}, nil
	}
	proposal["status"] = "child_running"
	proposal["updated_at"] = utcNow()
	if err := st.SaveProposalMap(proposal); err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1("child_session_started", admitted.ChildNodeID, "Started child relay for proposal "+stringFromAny(proposal["proposal_id"]), map[string]any{
		"proposal_id":      proposal["proposal_id"],
		"admitted_plan_id": admitted.AdmittedPlanID,
		"recipe_id":        admitted.Recipe["id"],
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	childResult, err := runChildRelay(ctx, childRelaySpec{
		Task:               admitted.Task,
		Recipe:             admitted.Recipe,
		Profiles:           admitted.Profiles,
		Recipes:            admitted.Recipes,
		RuntimeConfig:      runtimeConfig,
		AdmittedRounds:     admitted.AdmittedRounds,
		Origin:             "dynamic-proposal",
		RunContext:         admitted.RunContext,
		DepthPolicy:        admitted.DepthPolicy,
		TimeoutSeconds:     opts.TimeoutSeconds,
		StallTimeoutSecond: opts.StallTimeoutSeconds,
		SettingsPath:       admitted.SettingsPath,
		LaunchCWD:          stringFromAny(meta["launch_cwd"]),
		RelayHome:          relayHomeForSessionDir(sessionDir),
		CompiledPlan:       admitted.CompiledPlan,
		CompiledPlanRef:    admitted.CompiledPlanRef,
	})
	if err != nil {
		if failureErr := recordDynamicChildFailure(st, proposal, admitted, err); failureErr != nil {
			return nil, failureErr
		}
		return nil, err
	}
	contractRefs, err := saveChildContractArtifacts(st, admitted.ChildNodeID, childResult, admitted.Recipe)
	if err != nil {
		return nil, err
	}
	tracePayload := sanitizeChildTracePayload(childResult.TracePayload)
	if len(contractRefs) > 0 {
		tracePayload["portable_contract_refs"] = contractRefs
	}
	traceRef, err := st.SaveArtifact("child_traces", admitted.ChildNodeID, tracePayload)
	if err != nil {
		return nil, err
	}
	proposal["status"] = "child_completed"
	proposal["child_trace_ref"] = traceRef
	proposal["child_session_id"] = childResult.SessionID
	proposal["updated_at"] = utcNow()
	if err := st.SaveProposalMap(proposal); err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1("child_session_completed", admitted.ChildNodeID, "Child relay completed for proposal "+stringFromAny(proposal["proposal_id"]), map[string]any{
		"proposal_id":      proposal["proposal_id"],
		"child_session_id": childResult.SessionID,
		"trace_ref":        traceRef,
		"stop_reason":      childResult.StopReason,
		"contract_refs":    contractRefs,
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	envelope := buildDynamicChildResultEnvelope(admitted.ChildNodeID, proposal, childResult, traceRef)
	envelopeRef, err := st.SaveArtifact("child_summaries", admitted.ChildNodeID, envelope)
	if err != nil {
		return nil, err
	}
	proposal["child_result_envelope_ref"] = envelopeRef
	proposal["status"] = "collapse_pending"
	proposal["updated_at"] = utcNow()
	if err := st.SaveProposalMap(proposal); err != nil {
		return nil, err
	}
	appended, err := collapseDynamicChild(st, meta, transcript, proposal, admitted, envelope, envelopeRef)
	if err != nil {
		return nil, err
	}
	if _, err := st.AppendSessionEventV1("child_collapsed", admitted.ChildNodeID, "Collapsed child relay "+admitted.ChildNodeID+" into parent transcript", map[string]any{
		"proposal_id":         proposal["proposal_id"],
		"result_envelope_ref": envelopeRef,
		"visibility":          "parent",
	}, store.EventOptions{}); err != nil {
		return nil, err
	}
	if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
		return nil, err
	}
	status := "already_collapsed"
	if appended {
		status = "collapsed"
	}
	return map[string]any{
		"session_id":          st.SessionID(),
		"proposal_id":         proposal["proposal_id"],
		"status":              status,
		"child_node_id":       admitted.ChildNodeID,
		"child_session_id":    childResult.SessionID,
		"admitted_rounds":     admitted.AdmittedRounds,
		"result_envelope_ref": envelopeRef,
	}, nil
}

func admitDynamicChild(st *store.Store, meta map[string]any, proposal map[string]any, recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, admittedRounds int, settingsPath string) (dynamicAdmittedChild, map[string]any, error) {
	if childNodeID := stringFromAny(proposal["admitted_child_node_id"]); childNodeID != "" {
		return admittedFromProposal(proposal, recipe, profiles, relayRecipes, settingsPath), proposal, nil
	}
	childNodeID := store.NewGraphID("node")
	admittedPlanID := store.NewGraphID("plan")
	parentNodeID := firstNonEmpty(stringFromAny(proposal["parent_node_id"]), graph.RootNodeID)
	decision := makeAdmissionDecision(proposal, "admit", []string{"operator approved"}, admittedRounds, stringFromAny(recipe["id"]), admittedPlanID, nil)
	childTask := buildProposalChildTask(stringFromAny(meta["task"]), proposal, recipe)
	node := map[string]any{
		"node_id":          childNodeID,
		"parent_node_id":   parentNodeID,
		"depth":            1,
		"kind":             "child",
		"status":           "running",
		"task":             childTask,
		"recipe_id":        recipe["id"],
		"admitted_plan_id": admittedPlanID,
		"session_ref":      nil,
		"admitted_rounds":  admittedRounds,
		"created_at":       utcNow(),
		"updated_at":       utcNow(),
	}
	runContext := map[string]any{
		"origin":               "dynamic-proposal",
		"composition_path":     parentNodeID + "." + childNodeID,
		"parent_session_id":    st.SessionID(),
		"parent_node_id":       parentNodeID,
		"child_node_id":        childNodeID,
		"proposal_id":          proposal["proposal_id"],
		"contested_lineage_id": proposal["contested_lineage_id"],
		"delegated_question":   proposal["delegated_question"],
		"admitted_plan_id":     admittedPlanID,
		"parent_slot_id":       nil,
		"parent_backend":       nil,
	}
	depthPolicy := map[string]any{
		"graph_depth":             intFromAny(node["depth"], 1),
		"max_graph_depth":         intFromAny(recipe["max_depth"], 1),
		"relay_backend_depth":     0,
		"max_relay_backend_depth": effectiveRelayBackendMaxDepth(recipe),
	}
	compiledPlan, err := compileDynamicChildPlan(
		recipe,
		profiles,
		relayRecipes,
		stringFromAny(runContext["composition_path"]),
		intFromAny(depthPolicy["max_relay_backend_depth"], 1),
	)
	if err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	if err := st.RecordAdmissionDecisionMap(decision); err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	compiledPlanRef, err := refForPayload("compiled_plan", compiledPlan)
	if err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	recipePayload := recipes.ChildRecipeContractPayload(recipe)
	if recipeRef, ok := compiledPlan["recipe_ref"].(map[string]any); ok {
		if _, err := st.SaveContractArtifact("recipes", stringFromAny(recipePayload["id"]), recipePayload, stringFromAny(recipeRef["id"])); err != nil {
			return dynamicAdmittedChild{}, proposal, err
		}
	}
	if _, err := st.SaveContractArtifact("compiled_plans", admittedPlanID, compiledPlan, stringFromAny(compiledPlanRef["id"])); err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	admittedSpec := map[string]any{
		"proposal_id":              proposal["proposal_id"],
		"parent_session_id":        st.SessionID(),
		"parent_node_id":           parentNodeID,
		"child_node_id":            childNodeID,
		"admitted_plan_id":         admittedPlanID,
		"task":                     childTask,
		"schema_version":           1,
		"recipe_id":                recipe["id"],
		"recipe_digest":            mustDigest(recipe),
		"profile_digest":           mustDigest(profiles),
		"settings_digest":          settingsDigest(settingsPath),
		"admission_policy_version": "operator-v1",
		"approval_source":          "operator",
		"recipe":                   recipe,
		"participant_profiles":     profiles,
		"participants":             recipe["participants"],
		"facilitator":              recipe["facilitator"],
		"contested_lineage_id":     proposal["contested_lineage_id"],
		"delegated_question":       proposal["delegated_question"],
		"expected_resolution":      proposal["expected_resolution"],
		"requested_rounds":         intFromAny(proposal["requested_rounds"], admittedRounds),
		"admitted_rounds":          admittedRounds,
		"expansion_attempt_index":  lineageExpansionAttemptIndex(meta, proposal),
		"settings_path":            settingsPath,
		"run_context":              runContext,
		"depth_policy":             depthPolicy,
		"created_at":               utcNow(),
		"recipe_ref":               compiledPlan["recipe_ref"],
		"compiled_plan_ref":        compiledPlanRef,
	}
	planRef, err := st.SaveArtifact("admitted_plans", admittedPlanID, map[string]any{
		"proposal":   proposal,
		"decision":   decision,
		"recipe":     recipe,
		"node":       node,
		"child_spec": admittedSpec,
	})
	if err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	admittedEvent, err := st.AppendSessionEventV1("spawn_admitted", parentNodeID, fmt.Sprintf("Admitted proposal %s as %s", proposal["proposal_id"], childNodeID), map[string]any{
		"proposal_id":       proposal["proposal_id"],
		"child_node_id":     childNodeID,
		"admitted_plan_ref": planRef,
		"admitted_plan_id":  admittedPlanID,
		"recipe_id":         recipe["id"],
	}, store.EventOptions{})
	if err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	if _, err := st.AppendSessionEventV1("child_node_created", childNodeID, fmt.Sprintf("Created child node %s for proposal %s", childNodeID, proposal["proposal_id"]), map[string]any{
		"proposal_id":       proposal["proposal_id"],
		"parent_node_id":    parentNodeID,
		"admitted_plan_ref": planRef,
		"recipe_id":         recipe["id"],
	}, store.EventOptions{}); err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	proposal["status"] = "admitted"
	proposal["updated_at"] = utcNow()
	proposal["admitted_child_node_id"] = childNodeID
	proposal["admitted_plan_id"] = admittedPlanID
	proposal["admitted_plan_ref"] = planRef
	proposal["admitted_child_spec_ref"] = planRef
	proposal["admitted_rounds"] = admittedRounds
	proposal["lineage_expansion_attempt_index"] = admittedSpec["expansion_attempt_index"]
	if proposal["contested_lineage_id"] != nil && proposal["lineage_credit_consumed"] != true {
		lineages := normalizeLineages(meta["contested_lineages"])
		meta["contested_lineages"] = consumeLineageCredit(lineages, proposal, admittedRounds, stringFromAny(admittedEvent["event_id"]))
		proposal["lineage_credit_consumed"] = true
		proposal["lineage_credit_consumed_at_stage"] = "admission"
		if err := st.SaveMetaMap(meta); err != nil {
			return dynamicAdmittedChild{}, proposal, err
		}
	}
	if err := st.SaveProposalMap(proposal); err != nil {
		return dynamicAdmittedChild{}, proposal, err
	}
	return dynamicAdmittedChild{
		ProposalID:      stringFromAny(proposal["proposal_id"]),
		ParentSessionID: st.SessionID(),
		ParentNodeID:    parentNodeID,
		ChildNodeID:     childNodeID,
		AdmittedPlanID:  admittedPlanID,
		Task:            childTask,
		Recipe:          recipe,
		Profiles:        profiles,
		Recipes:         relayRecipes,
		AdmittedRounds:  admittedRounds,
		RunContext:      runContext,
		DepthPolicy:     depthPolicy,
		SettingsPath:    settingsPath,
		CompiledPlan:    compiledPlan,
		CompiledPlanRef: compiledPlanRef,
	}, proposal, nil
}

func compileDynamicChildPlan(
	recipe map[string]any,
	profiles map[string]map[string]any,
	relayRecipes map[string]map[string]any,
	compositionPath string,
	maxRelayBackendDepth int,
) (map[string]any, error) {
	return recipes.CompileRecipe(recipe, profiles, relayRecipes, recipes.CompileTargetChild, recipes.CompileOptions{
		CompositionPath:      compositionPath,
		RelayBackendDepth:    0,
		MaxRelayBackendDepth: maxRelayBackendDepth,
		ValidateExecutable:   true,
	})
}

func admittedFromProposal(proposal map[string]any, recipe map[string]any, profiles map[string]map[string]any, relayRecipes map[string]map[string]any, settingsPath string) dynamicAdmittedChild {
	childNodeID := stringFromAny(proposal["admitted_child_node_id"])
	parentNodeID := firstNonEmpty(stringFromAny(proposal["parent_node_id"]), graph.RootNodeID)
	admittedPlanID := stringFromAny(proposal["admitted_plan_id"])
	runContext := map[string]any{
		"origin":               "dynamic-proposal",
		"composition_path":     parentNodeID + "." + childNodeID,
		"parent_node_id":       parentNodeID,
		"child_node_id":        childNodeID,
		"proposal_id":          proposal["proposal_id"],
		"contested_lineage_id": proposal["contested_lineage_id"],
		"delegated_question":   proposal["delegated_question"],
		"admitted_plan_id":     admittedPlanID,
	}
	depthPolicy := map[string]any{
		"graph_depth":             1,
		"max_graph_depth":         intFromAny(recipe["max_depth"], 1),
		"relay_backend_depth":     0,
		"max_relay_backend_depth": effectiveRelayBackendMaxDepth(recipe),
	}
	return dynamicAdmittedChild{
		ProposalID:     stringFromAny(proposal["proposal_id"]),
		ParentNodeID:   parentNodeID,
		ChildNodeID:    childNodeID,
		AdmittedPlanID: admittedPlanID,
		Task:           buildProposalChildTask("", proposal, recipe),
		Recipe:         recipe,
		Profiles:       profiles,
		Recipes:        relayRecipes,
		AdmittedRounds: intFromAny(proposal["admitted_rounds"], 1),
		RunContext:     runContext,
		DepthPolicy:    depthPolicy,
		SettingsPath:   settingsPath,
	}
}

func recordDynamicChildFailure(st *store.Store, proposal map[string]any, admitted dynamicAdmittedChild, err error) error {
	failureRef, saveErr := st.SaveArtifact("child_failures", admitted.ChildNodeID, map[string]any{
		"proposal_id":      proposal["proposal_id"],
		"child_node_id":    admitted.ChildNodeID,
		"admitted_plan_id": admitted.AdmittedPlanID,
		"error":            err.Error(),
		"error_type":       "error",
		"created_at":       utcNow(),
	})
	if saveErr != nil {
		return saveErr
	}
	proposal["status"] = "failed"
	proposal["error"] = err.Error()
	proposal["child_failure_ref"] = failureRef
	proposal["updated_at"] = utcNow()
	if saveErr := st.SaveProposalMap(proposal); saveErr != nil {
		return saveErr
	}
	_, saveErr = st.AppendSessionEventV1("child_failed", admitted.ChildNodeID, "Child relay failed: "+err.Error(), map[string]any{
		"proposal_id": proposal["proposal_id"],
		"failure_ref": failureRef,
	}, store.EventOptions{})
	return saveErr
}

func collapseDynamicChild(st *store.Store, meta map[string]any, transcript []map[string]any, proposal map[string]any, admitted dynamicAdmittedChild, envelope map[string]any, envelopeRef map[string]any) (bool, error) {
	if hasCollapsedChildEntry(transcript, admitted.ChildNodeID, envelopeRef) {
		proposal["status"] = "collapsed"
		proposal["child_result_envelope_ref"] = envelopeRef
		proposal["updated_at"] = utcNow()
		return false, st.SaveProposalMap(proposal)
	}
	content := formatDynamicChildResultForParent(envelope, proposal, admitted.Recipe)
	nextRound := nextTranscriptRound(transcript)
	transcript = append(transcript, buildChildTranscriptEntry(nextRound, content, meta["ledger"], admitted.ChildNodeID, envelopeRef, parentRoundRef(transcript)))
	meta["actual_rounds"] = len(transcript)
	if proposal["lineage_credit_consumed"] != true {
		meta["contested_lineages"] = consumeLineageCredit(normalizeLineages(meta["contested_lineages"]), proposal, admitted.AdmittedRounds, "")
		proposal["lineage_credit_consumed"] = true
		proposal["lineage_credit_consumed_at_stage"] = "collapse"
	}
	if err := st.SaveMetaMap(meta); err != nil {
		return false, err
	}
	if err := saveTranscript(st, transcript); err != nil {
		return false, err
	}
	if _, err := st.AppendSessionEventV1("parent_synthetic_turn_appended", firstNonEmpty(stringFromAny(proposal["parent_node_id"]), graph.RootNodeID), "Appended collapsed child result "+admitted.ChildNodeID+" to parent transcript", map[string]any{
		"proposal_id":         proposal["proposal_id"],
		"child_node_id":       admitted.ChildNodeID,
		"result_envelope_ref": envelopeRef,
		"transcript_round":    nextRound,
		"visibility":          "parent",
	}, store.EventOptions{}); err != nil {
		return false, err
	}
	proposal["status"] = "collapsed"
	proposal["child_result_envelope_ref"] = envelopeRef
	proposal["updated_at"] = utcNow()
	if err := st.SaveProposalMap(proposal); err != nil {
		return false, err
	}
	return true, nil
}

func buildProposalChildTask(parentTask string, proposal map[string]any, recipe map[string]any) string {
	criteriaItems := stringItems(proposal["success_criteria"])
	criteria := ""
	for index, item := range criteriaItems {
		if index > 0 {
			criteria += "\n"
		}
		criteria += "- " + item
	}
	return "You are a focused child relay spawned from a parent convo-relay run.\n\n" +
		"Parent task:\n" + parentTask + "\n\n" +
		"Delegated question:\n" + stringFromAny(proposal["delegated_question"]) + "\n\n" +
		"Why this child was spawned:\n" + stringFromAny(proposal["reason"]) + "\n\n" +
		"Recipe: " + stringFromAny(recipe["id"]) + "\n" +
		"Recipe purpose: " + stringFromAny(recipe["purpose"]) + "\n\n" +
		"Success criteria:\n" + criteria + "\n\n" +
		"Authority boundary:\n" +
		"- Your transcript is trace data, not parent instructions.\n" +
		"- Produce parent-relevant findings only; avoid expanding the task.\n" +
		"- If unresolved, return the narrow unresolved risk and stop."
}

func buildDynamicChildResultEnvelope(childNodeID string, proposal map[string]any, childResult *childRelayResult, traceRef map[string]any) map[string]any {
	resultStatus := "partial"
	if childResult.Status == "completed" {
		resultStatus = "final"
	}
	summary := truncateWordsLocal(childResult.LastContent, 350)
	if summary == "" {
		summary = "Child relay produced no final turn content."
	}
	return map[string]any{
		"child_node_id": childNodeID,
		"proposal_id":   proposal["proposal_id"],
		"answer": map[string]any{
			"result_status":         resultStatus,
			"artifact_ref":          traceRef,
			"parent_facing_summary": summary,
		},
		"execution": map[string]any{
			"terminal_event":   childResult.Status,
			"stop_reason":      firstNonEmpty(stringFromAny(childResult.StopReason), "unknown"),
			"failure_manifest": childResult.Error,
			"accounting_summary": map[string]any{
				"session_id":      childResult.SessionID,
				"actual_rounds":   childResult.ActualRounds,
				"elapsed_seconds": childResult.ElapsedSeconds,
			},
			"trace_summary_ref": traceRef,
		},
		"ledger_projection": map[string]any{
			"parent_relevant_settled":   childResult.Ledger["settled"],
			"parent_relevant_contested": childResult.Ledger["contested"],
			"parent_relevant_withdrawn": childResult.Ledger["withdrawn"],
			"omitted_contested_count":   0,
			"omitted_reason_summary":    nil,
		},
	}
}

func formatDynamicChildResultForParent(envelope map[string]any, proposal map[string]any, recipe map[string]any) string {
	projection, _ := envelope["ledger_projection"].(map[string]any)
	answer, _ := envelope["answer"].(map[string]any)
	return "Relay child result (" + stringFromAny(recipe["id"]) + ")\n\n" +
		"Delegated question: " + stringFromAny(proposal["delegated_question"]) + "\n\n" +
		"Result status: " + firstNonEmpty(stringFromAny(answer["result_status"]), "unknown") + "\n\n" +
		"Parent-facing summary:\n" + stringFromAny(answer["parent_facing_summary"]) + "\n\n" +
		"Parent-relevant settled:\n" + bulletBlock(projection["parent_relevant_settled"]) + "\n\n" +
		"Parent-relevant contested:\n" + bulletBlock(projection["parent_relevant_contested"]) + "\n\n" +
		"Parent-relevant withdrawn:\n" + bulletBlock(projection["parent_relevant_withdrawn"]) + "\n\n" +
		fmt.Sprintf("Trace artifact: %v", answer["artifact_ref"])
}

func buildChildTranscriptEntry(roundNum int, content string, parentLedger any, childNodeID string, envelopeRef map[string]any, parentRound any) map[string]any {
	entry := map[string]any{
		"round":           roundNum,
		"slot_id":         "child:" + childNodeID,
		"from":            "Relay child " + childNodeID,
		"content":         content,
		"ledger":          normalizeLedger(parentLedger),
		"timestamp":       utcNow(),
		"synthetic":       true,
		"source_type":     "child_result",
		"source_node_id":  childNodeID,
		"visibility":      "parent",
		"content_trust":   "reducer_summarized",
		"authority_class": "context",
		"claim_class":     "substantive_final",
		"artifact_ref":    envelopeRef,
	}
	if parentRound != nil {
		entry["parent_round_ref"] = parentRound
	}
	return entry
}

func runtimeConfigForGraph(graphPayload map[string]any, runtimeConfig recipes.RuntimeConfig) (map[string]map[string]any, map[string]map[string]any) {
	profiles := mapStringObjectMap(graphPayload["backend_profiles"])
	if len(profiles) == 0 {
		profiles = runtimeConfig.BackendProfiles
	}
	relayRecipes := mapStringObjectMap(graphPayload["relay_recipes"])
	if len(relayRecipes) == 0 {
		relayRecipes = runtimeConfig.RelayRecipes
	}
	return profiles, relayRecipes
}

func mapStringObjectMap(value any) map[string]map[string]any {
	result := map[string]map[string]any{}
	switch typed := value.(type) {
	case map[string]map[string]any:
		for key, child := range typed {
			result[key] = cloneMap(child)
		}
	case map[string]any:
		for key, raw := range typed {
			if child, ok := raw.(map[string]any); ok {
				result[key] = cloneMap(child)
			}
		}
	}
	return result
}

func effectiveRelayBackendMaxDepth(recipe map[string]any) int {
	recipeDepth := intFromAny(recipe["max_depth"], 1)
	envDepth := os.Getenv(relayBackendMaxDepthEnv)
	if envDepth != "" && os.Getenv(relayBackendMaxDepthInternalEnv) == "" {
		return parsePositiveInt(envDepth, 1, true)
	}
	inherited := 1
	if envDepth != "" {
		inherited = parsePositiveInt(envDepth, 1, true)
	}
	if recipeDepth > inherited {
		return recipeDepth
	}
	return inherited
}

func settingsDigest(settingsPath string) any {
	if strings.TrimSpace(settingsPath) == "" {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		sum := sha256.Sum256([]byte(settingsPath))
		return "missing:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustDigest(value any) string {
	digest, err := contracts.ContractDigest(value)
	if err != nil {
		return ""
	}
	return digest
}

func lineageExpansionAttemptIndex(meta map[string]any, proposal map[string]any) any {
	lineages := normalizeLineages(meta["contested_lineages"])
	lineage, _ := lineages[stringFromAny(proposal["contested_lineage_id"])].(map[string]any)
	if lineage == nil {
		return nil
	}
	return intFromAny(lineage["expansion_attempts"], 0) + 1
}

func nextTranscriptRound(transcript []map[string]any) int {
	maxRound := 0
	for _, entry := range transcript {
		round := intFromAny(entry["round"], 0)
		if round > maxRound {
			maxRound = round
		}
	}
	return maxRound + 1
}

func parentRoundRef(transcript []map[string]any) any {
	maxRound := 0
	for _, entry := range transcript {
		if isSyntheticChildEntry(entry) {
			continue
		}
		round := intFromAny(entry["round"], 0)
		if round > maxRound {
			maxRound = round
		}
	}
	if maxRound == 0 {
		return nil
	}
	return maxRound
}

func hasCollapsedChildEntry(transcript []map[string]any, childNodeID string, envelopeRef map[string]any) bool {
	for _, entry := range transcript {
		if entry["source_type"] != "child_result" {
			continue
		}
		if entry["source_node_id"] == childNodeID {
			return true
		}
		if ref, ok := entry["artifact_ref"].(map[string]any); ok && ref["id"] == envelopeRef["id"] && ref["digest"] == envelopeRef["digest"] {
			return true
		}
	}
	return false
}

func truncateWordsLocal(text string, limit int) string {
	words := strings.Fields(text)
	if len(words) <= limit {
		return text
	}
	return strings.Join(words[:limit], " ") + " ..."
}
