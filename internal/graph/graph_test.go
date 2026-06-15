package graph

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestRepairFromEventsPreservesChildAndRelayBackendNodes(t *testing.T) {
	sessionDir := t.TempDir()
	writeEvents(t, sessionDir, []map[string]any{
		event(1, "evt_root", "node_started", "root", "Root relay node started", map[string]any{
			"session_ref":  "session-fixture",
			"dynamic_mode": "off",
		}),
		event(2, "evt_proposed", "spawn_proposed", "root", "Spawn proposed", map[string]any{
			"proposal_id":          "sp_fixture",
			"contested_lineage_id": "lineage-1",
			"recipe_id":            "review-panel",
		}),
		event(3, "evt_admitted", "spawn_admitted", "root", "Spawn admitted", map[string]any{
			"proposal_id":       "sp_fixture",
			"child_node_id":     "node_dynamic",
			"admitted_plan_ref": "plan-ref",
		}),
		event(4, "evt_child_node", "child_node_created", "node_dynamic", "Child node created", map[string]any{
			"parent_node_id":    "root",
			"recipe_id":         "review-panel",
			"admitted_plan_ref": "plan-ref",
		}),
		event(5, "evt_child_done", "child_session_completed", "node_dynamic", "Child completed", map[string]any{
			"child_session_id": "child-session",
			"trace_ref":        "artifacts/child_traces/child-session.json",
		}),
		event(6, "evt_relay_done", "relay_backend_child_completed", "node_relay", "Relay backend child completed", map[string]any{
			"parent_node_id":   "root",
			"slot_id":          "slot_1",
			"composition_path": "root.slot_1",
			"recipe_id":        "nested-review",
			"child_session_id": "nested-session",
			"trace_ref":        "artifacts/child_traces/nested-session.json",
			"contract_refs":    map[string]any{"child_result_ref": artifactRef("child_result:test")},
		}),
	})
	writeArtifactIndex(t, sessionDir)

	repaired, events, err := RepairFromEvents(store.New(sessionDir))
	if err != nil {
		t.Fatalf("repair graph: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("event count = %d, want 6", len(events))
	}

	nodes := repaired["nodes"].(map[string]any)
	root := nodes["root"].(map[string]any)
	if root["kind"] != "root" || root["status"] != "running" {
		t.Fatalf("root node = %#v", root)
	}
	dynamic := nodes["node_dynamic"].(map[string]any)
	if dynamic["kind"] != "child" || dynamic["status"] != "completed" || dynamic["session_ref"] != "child-session" {
		t.Fatalf("dynamic child node = %#v", dynamic)
	}
	relay := nodes["node_relay"].(map[string]any)
	if relay["kind"] != "relay_backend_child" || relay["composition_path"] != "root.slot_1" {
		t.Fatalf("relay backend node = %#v", relay)
	}
	if relay["contract_refs"].(map[string]any)["child_result_ref"] == nil {
		t.Fatalf("relay backend node missing contract refs: %#v", relay)
	}

	edges := repaired["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edge count = %d, want 2: %#v", len(edges), edges)
	}
	artifacts := repaired["artifacts"].(map[string]any)
	if _, ok := artifacts["child_results/result"]; !ok {
		t.Fatalf("artifact graph entries = %#v", artifacts)
	}
}

func TestLoadReadsGraphSnapshot(t *testing.T) {
	sessionDir := t.TempDir()
	graphPath := filepath.Join(sessionDir, store.GraphFilename)
	body := []byte(`{"version":1,"root_node_id":"root","nodes":{"root":{"node_id":"root","status":"completed"}},"edges":[]}`)
	if err := os.WriteFile(graphPath, body, 0o644); err != nil {
		t.Fatalf("write graph: %v", err)
	}

	loaded := Load(store.New(sessionDir))
	nodes := loaded["nodes"].(map[string]any)
	root := nodes["root"].(map[string]any)
	if root["status"] != "completed" {
		t.Fatalf("loaded graph root = %#v", root)
	}
	if loaded["proposals"] == nil || loaded["artifacts"] == nil {
		t.Fatalf("loaded graph did not fill default sections: %#v", loaded)
	}
}

func TestRepairAndSaveFromEventsWritesGraphSnapshot(t *testing.T) {
	sessionDir := t.TempDir()
	writeEvents(t, sessionDir, []map[string]any{
		event(1, "evt_root", "node_started", "root", "Root started", map[string]any{
			"session_ref":  "session-fixture",
			"dynamic_mode": "off",
		}),
		event(2, "evt_done", "node_completed", "root", "Root completed", map[string]any{}),
	})
	st := store.New(sessionDir)
	repaired, events, err := RepairAndSaveFromEvents(st)
	if err != nil {
		t.Fatalf("repair and save: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	loaded := Load(st)
	if loaded["nodes"].(map[string]any)["root"].(map[string]any)["status"] != "completed" {
		t.Fatalf("saved graph root = %#v", loaded["nodes"])
	}
	if repaired["nodes"].(map[string]any)["root"].(map[string]any)["status"] != "completed" {
		t.Fatalf("repaired graph root = %#v", repaired["nodes"])
	}
}

func TestSummaryIncludesNodesAndProposals(t *testing.T) {
	graph := DefaultGraph()
	graph["nodes"] = map[string]any{
		"root": map[string]any{"depth": 0, "status": "completed", "recipe_id": nil},
	}
	graph["proposals"] = map[string]any{
		"sp_fixture": map[string]any{"status": "admitted", "selected_recipe_id": "review-panel"},
	}
	summary := Summary(graph)
	for _, want := range []string{
		"Graph v1: 1 node(s), 1 proposal(s)",
		"root  depth=0  status=completed  recipe=root",
		"sp_fixture  status=admitted  recipe=review-panel",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func event(seq int, eventID string, eventType string, nodeID string, summary string, payload map[string]any) map[string]any {
	return map[string]any{
		"kind":           "session_event",
		"schema_version": 1,
		"seq":            seq,
		"event_id":       eventID,
		"timestamp":      "2026-05-19T00:00:00+00:00",
		"event_type":     eventType,
		"node_id":        nodeID,
		"visibility":     "operator",
		"summary":        summary,
		"payload":        payload,
	}
}

func artifactRef(id string) map[string]any {
	return map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             id,
		"digest":         "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func writeEvents(t *testing.T, sessionDir string, events []map[string]any) {
	t.Helper()
	lines := make([][]byte, 0, len(events))
	for _, rawEvent := range events {
		validated, err := contracts.ValidateSessionEvent(rawEvent)
		if err != nil {
			t.Fatalf("validate event: %v", err)
		}
		line, err := contracts.CanonicalJSONBytes(validated)
		if err != nil {
			t.Fatalf("canonical event: %v", err)
		}
		lines = append(lines, line)
	}
	body := bytes.Join(lines, []byte("\n"))
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(sessionDir, store.EventsFilename), body, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
}

func writeArtifactIndex(t *testing.T, sessionDir string) {
	t.Helper()
	index := map[string]any{
		"kind":           "artifact_index",
		"schema_version": 1,
		"entries": []any{
			map[string]any{
				"ref":  artifactRef("child_result:test"),
				"path": "artifacts/child_results/result.json",
			},
		},
	}
	body, err := contracts.CanonicalJSONBytes(index)
	if err != nil {
		t.Fatalf("canonical index: %v", err)
	}
	path := filepath.Join(sessionDir, "artifacts", store.ArtifactIndexFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write artifact index: %v", err)
	}
}
