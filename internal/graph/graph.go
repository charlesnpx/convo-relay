package graph

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/store"
)

const (
	GraphVersion = 1
	RootNodeID   = "root"
)

func DefaultGraph() map[string]any {
	return map[string]any{
		"version":             GraphVersion,
		"root_node_id":        RootNodeID,
		"nodes":               map[string]any{},
		"edges":               []any{},
		"proposals":           map[string]any{},
		"admission_decisions": map[string]any{},
		"slot_replacements":   []any{},
		"artifacts":           map[string]any{},
		"backend_profiles":    map[string]any{},
		"relay_recipes":       map[string]any{},
	}
}

func Load(st *store.Store) map[string]any {
	return st.LoadGraph()
}

func RepairFromEvents(st *store.Store) (map[string]any, []map[string]any, error) {
	events, err := st.ReadEvents()
	if err != nil {
		return nil, nil, err
	}

	graph := DefaultGraph()
	for _, event := range events {
		applyEvent(graph, event)
	}
	graph["artifacts"] = artifactGraphEntries(st.ArtifactIndex())
	mergeProposalSidecars(graph, st)
	return graph, events, nil
}

func RepairAndSaveFromEvents(st *store.Store) (map[string]any, []map[string]any, error) {
	repaired, events, err := RepairFromEvents(st)
	if err != nil {
		return nil, nil, err
	}
	if err := st.SaveGraph(repaired); err != nil {
		return nil, nil, err
	}
	return repaired, events, nil
}

func Summary(graph map[string]any) string {
	nodes, _ := graph["nodes"].(map[string]any)
	proposals, _ := graph["proposals"].(map[string]any)
	lines := []string{
		fmt.Sprintf("Graph v%v: %d node(s), %d proposal(s)", valueOr(graph["version"], "?"), len(nodes), len(proposals)),
	}
	if replacements := asSlice(graph["slot_replacements"]); len(replacements) > 0 {
		lines = append(lines, fmt.Sprintf("Slot replacements: %d", len(replacements)))
	}

	nodeIDs := sortedKeys(nodes)
	for _, nodeID := range nodeIDs {
		node, _ := nodes[nodeID].(map[string]any)
		recipe := valueOr(node["recipe_id"], "root")
		if recipe == nil || recipe == "" {
			recipe = "root"
		}
		lines = append(lines, fmt.Sprintf(
			"  %s  depth=%v  status=%v  recipe=%v",
			nodeID,
			valueOr(node["depth"], "?"),
			valueOr(node["status"], "?"),
			recipe,
		))
	}

	if len(proposals) > 0 {
		lines = append(lines, "")
		lines = append(lines, "Proposals:")
		for _, proposalID := range sortedKeys(proposals) {
			proposal, _ := proposals[proposalID].(map[string]any)
			lines = append(lines, fmt.Sprintf(
				"  %s  status=%v  recipe=%v",
				proposalID,
				valueOr(proposal["status"], "?"),
				valueOr(proposal["selected_recipe_id"], "?"),
			))
		}
	}
	return strings.Join(lines, "\n")
}

func applyEvent(graph map[string]any, event map[string]any) {
	eventType, _ := event["event_type"].(string)
	payload, _ := event["payload"].(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}
	timestamp := event["timestamp"]
	nodeID, _ := event["node_id"].(string)
	if nodeID == "" {
		nodeID = RootNodeID
	}

	switch eventType {
	case "node_started":
		nodes(graph)[nodeID] = setDefaultObject(nodes(graph), nodeID, map[string]any{
			"node_id":        nodeID,
			"parent_node_id": nil,
			"depth":          0,
			"kind":           nodeKindForStart(nodeID),
			"status":         "running",
			"session_ref":    payload["session_ref"],
			"dynamic_mode":   payload["dynamic_mode"],
			"created_at":     timestamp,
			"updated_at":     timestamp,
		})
	case "node_completed", "node_interrupted", "node_failed":
		node := setDefaultObject(nodes(graph), nodeID, map[string]any{"node_id": nodeID})
		node["status"] = map[string]string{
			"node_completed":   "completed",
			"node_interrupted": "interrupted",
			"node_failed":      "failed",
		}[eventType]
		node["updated_at"] = timestamp
	case "spawn_proposed":
		proposalID, _ := payload["proposal_id"].(string)
		if proposalID != "" {
			proposals(graph)[proposalID] = map[string]any{
				"proposal_id":          proposalID,
				"parent_node_id":       nodeID,
				"contested_lineage_id": payload["contested_lineage_id"],
				"selected_recipe_id":   payload["recipe_id"],
				"status":               "proposed",
				"updated_at":           timestamp,
			}
		}
	case "spawn_admitted":
		proposalID, _ := payload["proposal_id"].(string)
		if proposalID != "" {
			proposal := setDefaultObject(proposals(graph), proposalID, map[string]any{"proposal_id": proposalID})
			proposal["status"] = "admitted"
			proposal["child_node_id"] = payload["child_node_id"]
			proposal["admitted_plan_ref"] = payload["admitted_plan_ref"]
			proposal["updated_at"] = timestamp
		}
	case "child_node_created":
		parentNodeID, _ := payload["parent_node_id"].(string)
		if parentNodeID == "" {
			parentNodeID = RootNodeID
		}
		graphDepth := nodeDepth(graph, parentNodeID) + 1
		node := map[string]any{
			"node_id":           nodeID,
			"parent_node_id":    parentNodeID,
			"depth":             graphDepth,
			"kind":              "child",
			"status":            "running",
			"recipe_id":         payload["recipe_id"],
			"admitted_plan_ref": payload["admitted_plan_ref"],
			"updated_at":        timestamp,
		}
		nodes(graph)[nodeID] = node
		appendEdge(graph, map[string]any{"from": parentNodeID, "to": nodeID, "kind": "spawned"})
	case "child_session_completed":
		node := setDefaultObject(nodes(graph), nodeID, map[string]any{"node_id": nodeID, "kind": "child"})
		node["status"] = "completed"
		node["session_ref"] = payload["child_session_id"]
		node["trace_ref"] = payload["trace_ref"]
		node["updated_at"] = timestamp
	case "relay_backend_child_completed":
		parentNodeID, _ := payload["parent_node_id"].(string)
		if parentNodeID == "" {
			parentNodeID = RootNodeID
		}
		graphDepth := nodeDepth(graph, parentNodeID) + 1
		node := setDefaultObject(nodes(graph), nodeID, map[string]any{
			"node_id":        nodeID,
			"parent_node_id": parentNodeID,
			"depth":          graphDepth,
			"kind":           "relay_backend_child",
		})
		node["status"] = "completed"
		node["slot_id"] = payload["slot_id"]
		node["composition_path"] = payload["composition_path"]
		node["recipe_id"] = payload["recipe_id"]
		node["child_session_id"] = payload["child_session_id"]
		node["session_ref"] = payload["child_session_id"]
		node["trace_ref"] = payload["trace_ref"]
		node["contract_refs"] = payload["contract_refs"]
		node["updated_at"] = timestamp
		addEdgeIfMissing(graph, map[string]any{"from": parentNodeID, "to": nodeID, "kind": "relay_backend_child"})
	case "child_failed":
		node := setDefaultObject(nodes(graph), nodeID, map[string]any{"node_id": nodeID, "kind": "child"})
		node["status"] = "failed"
		node["failure_ref"] = payload["failure_ref"]
		node["updated_at"] = timestamp
	case "child_collapsed":
		node := setDefaultObject(nodes(graph), nodeID, map[string]any{"node_id": nodeID, "kind": "child"})
		node["result_envelope_ref"] = payload["result_envelope_ref"]
		node["updated_at"] = timestamp
	case "slot_replaced":
		replacement := cloneObject(payload)
		replacement["event_id"] = event["event_id"]
		replacement["timestamp"] = timestamp
		graph["slot_replacements"] = append(asSlice(graph["slot_replacements"]), replacement)
	}
}

func artifactGraphEntries(index map[string]any) map[string]any {
	artifacts := map[string]any{}
	entries, _ := index["entries"].([]any)
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		artifactPath, _ := entry["path"].(string)
		if !strings.HasPrefix(artifactPath, "artifacts/") || strings.HasSuffix(artifactPath, "/"+store.ArtifactIndexFilename) {
			continue
		}
		parts := strings.Split(artifactPath, "/")
		if len(parts) < 3 {
			continue
		}
		category := parts[1]
		artifactID := strings.TrimSuffix(path.Base(parts[len(parts)-1]), path.Ext(parts[len(parts)-1]))
		artifacts[category+"/"+artifactID] = map[string]any{
			"artifact_id": artifactID,
			"category":    category,
			"path":        artifactPath,
			"ref":         entry["ref"],
		}
	}
	return artifacts
}

func mergeProposalSidecars(graph map[string]any, st *store.Store) {
	for _, proposal := range mustListProposals(st) {
		proposalID, _ := proposal["proposal_id"].(string)
		if proposalID == "" {
			continue
		}
		proposals(graph)[proposalID] = map[string]any{
			"proposal_id":          proposalID,
			"parent_node_id":       proposal["parent_node_id"],
			"contested_lineage_id": proposal["contested_lineage_id"],
			"selected_recipe_id":   proposal["selected_recipe_id"],
			"status":               proposal["status"],
			"delegated_question":   proposal["delegated_question"],
			"updated_at":           proposal["updated_at"],
		}
	}
	existing := st.LoadGraph()
	if decisions, ok := existing["admission_decisions"].(map[string]any); ok && len(decisions) > 0 {
		graph["admission_decisions"] = decisions
	}
}

func mustListProposals(st *store.Store) []map[string]any {
	proposals, err := st.ListProposalMaps()
	if err != nil {
		return nil
	}
	return proposals
}

func nodes(graph map[string]any) map[string]any {
	return objectMap(graph, "nodes")
}

func proposals(graph map[string]any) map[string]any {
	return objectMap(graph, "proposals")
}

func edges(graph map[string]any) []any {
	existing, ok := graph["edges"].([]any)
	if !ok {
		existing = []any{}
		graph["edges"] = existing
	}
	return existing
}

func objectMap(graph map[string]any, key string) map[string]any {
	existing, ok := graph[key].(map[string]any)
	if !ok {
		existing = map[string]any{}
		graph[key] = existing
	}
	return existing
}

func setDefaultObject(container map[string]any, key string, defaults map[string]any) map[string]any {
	if existing, ok := container[key].(map[string]any); ok {
		return existing
	}
	container[key] = defaults
	return defaults
}

func nodeKindForStart(nodeID string) string {
	if nodeID == RootNodeID {
		return "root"
	}
	return "child"
}

func nodeDepth(graph map[string]any, nodeID string) int {
	node, _ := nodes(graph)[nodeID].(map[string]any)
	return intFromAny(node["depth"], 0)
}

func addEdgeIfMissing(graph map[string]any, edge map[string]any) {
	current := edges(graph)
	for _, rawExisting := range current {
		existing, ok := rawExisting.(map[string]any)
		if !ok {
			continue
		}
		if existing["from"] == edge["from"] && existing["to"] == edge["to"] && existing["kind"] == edge["kind"] {
			return
		}
	}
	current = append(current, edge)
	graph["edges"] = current
}

func appendEdge(graph map[string]any, edge map[string]any) {
	graph["edges"] = append(edges(graph), edge)
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return fallback
	}
}

func asSlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

func cloneObject(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func valueOr(value any, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
