package runner

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const (
	defaultLineageRoundCredit = 8
	defaultLineageSpawnCredit = 2
)

var activeProposalStatuses = map[string]bool{
	"proposed":         true,
	"admitted":         true,
	"running":          true,
	"child_running":    true,
	"child_completed":  true,
	"collapse_pending": true,
	"collapsed":        true,
}

func normalizeDynamicMode(value any) string {
	switch strings.TrimSpace(stringFromAny(value)) {
	case "ask", "auto-safe":
		return strings.TrimSpace(stringFromAny(value))
	default:
		return defaultDynamicMode
	}
}

func updateContestedLineages(rawLineages any, previousLedger any, currentLedger any, roundNum int, eventID string) map[string]any {
	lineages := normalizeLineages(rawLineages)
	previous := normalizeLedger(previousLedger)
	current := normalizeLedger(currentLedger)
	currentContested := stringSet(current["contested"])
	currentSettled := stringSet(current["settled"])
	currentWithdrawn := stringSet(current["withdrawn"])
	previousContested := stringSet(previous["contested"])

	for _, rawItem := range asSlice(current["contested"]) {
		item := strings.TrimSpace(stringFromAny(rawItem))
		if item == "" {
			continue
		}
		lineageID := lineageIDForItem(item)
		if existing, ok := lineages[lineageID].(map[string]any); ok {
			existing["latest_item"] = item
			continue
		}
		lineages[lineageID] = map[string]any{
			"lineage_id":             lineageID,
			"root_item":              item,
			"latest_item":            item,
			"parent_lineage_id":      nil,
			"status":                 "open",
			"depth":                  0,
			"expansion_attempts":     0,
			"remaining_round_credit": defaultLineageRoundCredit,
			"remaining_spawn_credit": defaultLineageSpawnCredit,
			"sibling_split_count":    0,
			"history": []any{
				map[string]any{
					"event_id": eventID,
					"round":    roundNum,
					"action":   "created",
					"item":     item,
					"ts":       utcNow(),
				},
			},
		}
	}

	for _, rawLineage := range lineages {
		lineage, ok := rawLineage.(map[string]any)
		if !ok {
			continue
		}
		status := stringFromAny(lineage["status"])
		if status != "open" && status != "promoted" {
			continue
		}
		item := firstNonEmpty(stringFromAny(lineage["latest_item"]), stringFromAny(lineage["root_item"]))
		if item == "" {
			continue
		}
		switch {
		case currentContested[item]:
			lineage["status"] = "open"
		case currentSettled[item]:
			markLineage(lineage, "resolved", "resolved", roundNum, eventID)
		case currentWithdrawn[item]:
			markLineage(lineage, "withdrawn", "withdrawn", roundNum, eventID)
		case previousContested[item]:
			markLineage(lineage, "promoted", "promoted", roundNum, eventID)
		}
	}
	return lineages
}

func maybeCreateSpawnProposals(st *store.Store, dynamicMode string, lineages map[string]any, previousLedger any, currentLedger any, roundNum int, triggerEventID string, profiles map[string]map[string]any, relayRecipes map[string]map[string]any) ([]map[string]any, error) {
	if normalizeDynamicMode(dynamicMode) == defaultDynamicMode {
		return nil, nil
	}
	previous := normalizeLedger(previousLedger)
	current := normalizeLedger(currentLedger)
	previousContested := stringSet(previous["contested"])
	existing, err := st.ListProposalMaps()
	if err != nil {
		return nil, err
	}
	activeLineages := map[string]bool{}
	for _, proposal := range existing {
		if activeProposalStatuses[stringFromAny(proposal["status"])] {
			activeLineages[stringFromAny(proposal["contested_lineage_id"])] = true
		}
	}
	proposals := []map[string]any{}
	for _, rawItem := range asSlice(current["contested"]) {
		item := strings.TrimSpace(stringFromAny(rawItem))
		if item == "" || !previousContested[item] || roundNum < 2 {
			continue
		}
		lineageID := lineageIDForItem(item)
		if activeLineages[lineageID] {
			continue
		}
		lineage, _ := lineages[lineageID].(map[string]any)
		if lineage == nil || intFromAny(lineage["remaining_spawn_credit"], 0) <= 0 {
			continue
		}
		recipe, rejected := selectRecipeForItem(item, relayRecipes)
		if recipe == nil {
			continue
		}
		requestedRounds := minPositiveInt(intFromAny(recipe["max_rounds"], 1), intFromAny(lineage["remaining_round_credit"], 1))
		proposal := buildSpawnProposal(item, lineage, graph.RootNodeID, triggerEventID, recipe, rejected, requestedRounds)
		decision := makeInitialAdmissionDecision(proposal, dynamicMode, recipe)
		if decision["decision"] == "admit" {
			if _, err := compileDynamicChildPlan(
				recipe,
				profiles,
				relayRecipes,
				"root.dynamic."+stringFromAny(proposal["proposal_id"]),
				effectiveRelayBackendMaxDepth(recipe),
			); err != nil {
				return nil, err
			}
			proposal["status"] = "admitted"
		}
		if err := st.SaveProposalMap(proposal); err != nil {
			return nil, err
		}
		if err := st.RecordAdmissionDecisionMap(decision); err != nil {
			return nil, err
		}
		if _, err := st.AppendSessionEventV1(
			"spawn_proposed",
			graph.RootNodeID,
			fmt.Sprintf("Proposed %s for %s", proposal["selected_recipe_id"], lineageID),
			map[string]any{
				"proposal_id":          proposal["proposal_id"],
				"contested_lineage_id": lineageID,
				"recipe_id":            proposal["selected_recipe_id"],
			},
			store.EventOptions{},
		); err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, nil
}

func buildSpawnProposal(contestedItem string, lineage map[string]any, parentNodeID string, triggerEventID string, recipe map[string]any, rejected []map[string]string, requestedRounds int) map[string]any {
	lineageID := stringFromAny(lineage["lineage_id"])
	question := "Resolve this contested parent-relay item: " + contestedItem
	rejectedItems := make([]any, 0, len(rejected))
	for _, item := range rejected {
		rejectedItems = append(rejectedItems, map[string]any{"recipe_id": item["recipe_id"], "reason": item["reason"]})
	}
	return map[string]any{
		"proposal_id":          store.NewGraphID("sp"),
		"parent_node_id":       parentNodeID,
		"trigger_event_id":     triggerEventID,
		"proposed_by":          "facilitator",
		"contested_lineage_id": lineageID,
		"delegated_question":   question,
		"reason":               "Contested item persisted across a completed parent exchange: " + contestedItem,
		"expected_resolution":  "Return either a concrete resolution, or an attributed unresolved risk the parent can decide on.",
		"success_criteria": []any{
			"Answer the delegated question directly.",
			"Identify evidence or code paths that support the answer.",
			"Name any remaining disagreement as parent-relevant contested output.",
		},
		"selected_recipe_id":     recipe["id"],
		"rejected_recipe_ids":    rejectedItems,
		"requested_depth":        intFromAny(lineage["depth"], 0) + 1,
		"requested_rounds":       requestedRounds,
		"requested_participants": recipe["participants"],
		"status":                 "proposed",
		"created_at":             utcNow(),
		"updated_at":             utcNow(),
	}
}

func makeInitialAdmissionDecision(proposal map[string]any, dynamicMode string, recipe map[string]any) map[string]any {
	if stringFromAny(recipe["origin"]) == "generated" {
		return makeAdmissionDecision(proposal, "require_user", []string{"generated recipe requires operator review"}, 0, "", "", []string{stringFromAny(proposal["proposal_id"])})
	}
	if dynamicMode == "auto-safe" && recipe["auto_approval"] == "auto-safe" {
		return makeAdmissionDecision(proposal, "admit", []string{"auto-safe policy admitted a recipe marked auto-safe"}, intFromAny(proposal["requested_rounds"], 1), stringFromAny(recipe["id"]), store.NewGraphID("plan"), nil)
	}
	return makeAdmissionDecision(proposal, "require_user", []string{"dynamic expansion requires operator approval"}, 0, "", "", []string{stringFromAny(proposal["proposal_id"])})
}

func makeAdmissionDecision(proposal map[string]any, decision string, reasons []string, admittedRounds int, admittedRecipeID string, admittedChildPlanID string, requiredAckIDs []string) map[string]any {
	reasonItems := make([]any, 0, len(reasons))
	for _, reason := range reasons {
		reasonItems = append(reasonItems, reason)
	}
	payload := map[string]any{
		"proposal_id": proposal["proposal_id"],
		"decision":    decision,
		"reasons":     reasonItems,
		"created_at":  utcNow(),
	}
	if admittedChildPlanID != "" {
		payload["admitted_child_plan_id"] = admittedChildPlanID
	}
	if admittedRounds > 0 {
		payload["admitted_rounds"] = admittedRounds
	}
	if admittedRecipeID != "" {
		payload["admitted_recipe_id"] = admittedRecipeID
	}
	if len(requiredAckIDs) > 0 {
		items := make([]any, 0, len(requiredAckIDs))
		for _, id := range requiredAckIDs {
			items = append(items, id)
		}
		payload["required_ack_ids"] = items
	}
	return payload
}

func consumeLineageCredit(lineages map[string]any, proposal map[string]any, admittedRounds int, eventID string) map[string]any {
	lineageID := stringFromAny(proposal["contested_lineage_id"])
	lineage, _ := lineages[lineageID].(map[string]any)
	if lineage == nil {
		return lineages
	}
	lineage["expansion_attempts"] = intFromAny(lineage["expansion_attempts"], 0) + 1
	lineage["remaining_spawn_credit"] = maxInt(intFromAny(lineage["remaining_spawn_credit"], 0)-1, 0)
	lineage["remaining_round_credit"] = maxInt(intFromAny(lineage["remaining_round_credit"], 0)-admittedRounds, 0)
	history := asSlice(lineage["history"])
	history = append(history, map[string]any{
		"event_id":        eventID,
		"action":          "spawned",
		"proposal_id":     proposal["proposal_id"],
		"admitted_rounds": admittedRounds,
		"ts":              utcNow(),
	})
	lineage["history"] = history
	return lineages
}

func normalizeLineages(raw any) map[string]any {
	result := map[string]any{}
	if object, ok := raw.(map[string]any); ok {
		for _, value := range object {
			if lineage, ok := value.(map[string]any); ok {
				lineageID := stringFromAny(lineage["lineage_id"])
				if lineageID != "" {
					result[lineageID] = cloneMap(lineage)
				}
			}
		}
		return result
	}
	for _, value := range asSlice(raw) {
		if lineage, ok := value.(map[string]any); ok {
			lineageID := stringFromAny(lineage["lineage_id"])
			if lineageID != "" {
				result[lineageID] = cloneMap(lineage)
			}
		}
	}
	return result
}

func markLineage(lineage map[string]any, status string, action string, roundNum int, eventID string) {
	if lineage["status"] == status {
		return
	}
	lineage["status"] = status
	history := asSlice(lineage["history"])
	history = append(history, map[string]any{
		"event_id": eventID,
		"round":    roundNum,
		"action":   action,
		"ts":       utcNow(),
	})
	lineage["history"] = history
}

func selectRecipeForItem(item string, relayRecipes map[string]map[string]any) (map[string]any, []map[string]string) {
	lowered := strings.ToLower(item)
	rejected := []map[string]string{}
	for _, recipeID := range sortedRecipeIDs(relayRecipes) {
		recipe := relayRecipes[recipeID]
		keywords := stringItems(recipe["match_keywords"])
		if len(keywords) > 0 {
			for _, keyword := range keywords {
				if strings.Contains(lowered, strings.ToLower(keyword)) {
					return recipe, rejected
				}
			}
			rejected = append(rejected, map[string]string{"recipe_id": stringFromAny(recipe["id"]), "reason": "keywords did not match contested item"})
		}
	}
	if recipe, ok := relayRecipes["review-panel"]; ok {
		return recipe, rejected
	}
	for _, recipeID := range sortedRecipeIDs(relayRecipes) {
		return relayRecipes[recipeID], rejected
	}
	return nil, rejected
}

func lineageIDForItem(item string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(item)), " ")
	sum := sha1.Sum([]byte(normalized))
	return "lin_" + hex.EncodeToString(sum[:])[:10]
}

func stringSet(value any) map[string]bool {
	result := map[string]bool{}
	for _, item := range asSlice(value) {
		text := strings.TrimSpace(stringFromAny(item))
		if text != "" {
			result[text] = true
		}
	}
	return result
}

func stringItems(value any) []string {
	items := []string{}
	for _, item := range asSlice(value) {
		text := strings.TrimSpace(stringFromAny(item))
		if text != "" {
			items = append(items, text)
		}
	}
	return items
}

func sortedRecipeIDs(relayRecipes map[string]map[string]any) []string {
	ids := make([]string, 0, len(relayRecipes))
	for recipeID := range relayRecipes {
		ids = append(ids, recipeID)
	}
	sort.Strings(ids)
	return ids
}

func minPositiveInt(a int, b int) int {
	if a <= 0 {
		a = 1
	}
	if b <= 0 {
		b = 1
	}
	if a < b {
		return a
	}
	return b
}
