package retirement_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/runner"
	"github.com/charlesnpx/convo-relay/internal/store"
)

const phase13Timestamp = "2026-05-19T00:00:00+00:00"

type corpusSession struct {
	name string
	dir  string
}

type treeSnapshot map[string][]byte

func TestPhase13HistoricalCorpusReadOnlyCommandsDoNotMutate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "relay-home")
	corpus := buildPhase13Corpus(t, home)

	for _, session := range corpus {
		t.Run(session.name, func(t *testing.T) {
			before := snapshotTree(t, session.dir)

			if _, err := inspect.BuildShowTranscriptReport(session.dir, 0, ""); err != nil {
				t.Fatalf("show transcript: %v", err)
			}
			if _, err := inspect.BuildShowGraphReport(session.dir); err != nil {
				t.Fatalf("show graph: %v", err)
			}
			if _, err := inspect.BuildContractsReport(session.dir, false, "", ""); err != nil {
				t.Fatalf("contracts: %v", err)
			}
			if _, err := inspect.RenderDiff(session.dir); err != nil {
				t.Fatalf("diff: %v", err)
			}
			if _, err := inspect.BuildDisplayHTML(session.dir); err != nil {
				t.Fatalf("display html: %v", err)
			}

			after := snapshotTree(t, session.dir)
			assertSnapshotEqual(t, before, after)
		})
	}

	t.Run("list sessions is read-only", func(t *testing.T) {
		before := snapshotTree(t, home)
		if _, err := runner.ListSessions(home, 500); err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		after := snapshotTree(t, home)
		assertSnapshotEqual(t, before, after)
	})
}

func TestPhase13MutatingCommandsDeclareOwnershipAndAreIdempotent(t *testing.T) {
	t.Run("graph repair owns only graph snapshot and is idempotent", func(t *testing.T) {
		sessionDir := filepath.Join(t.TempDir(), "python-era-graph-repair")
		createPhase13Session(t, sessionDir, "python-era-graph-repair", map[string]any{
			"slots": []any{
				slotMeta("claude", "slot_0", map[string]any{"session_id": "claude-python", "cwd": sessionDir}),
				slotMeta("gemini", "slot_1", map[string]any{"session_ref": "gemini-python", "cwd": sessionDir}),
			},
			"python_unknown_meta": map[string]any{"kept": true},
		}, phase13Transcript("Claude", "Gemini"))
		st := store.New(sessionDir)
		appendRootEvents(t, st, "python-era-graph-repair")
		if err := st.SaveGraph(map[string]any{
			"nodes": map[string]any{
				"stale_python_node": map[string]any{"status": "stale"},
			},
			"edges": []any{},
		}); err != nil {
			t.Fatalf("save stale graph: %v", err)
		}

		before := snapshotTree(t, sessionDir)
		if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
			t.Fatalf("repair graph: %v", err)
		}
		afterFirst := snapshotTree(t, sessionDir)
		assertChangedFiles(t, before, afterFirst, []string{"graph.json"})
		meta := readJSONObjectFile(t, filepath.Join(sessionDir, "meta.json"))
		if _, ok := meta["python_unknown_meta"].(map[string]any); !ok {
			t.Fatalf("graph repair dropped unknown meta field: %#v", meta)
		}

		if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
			t.Fatalf("repair graph second pass: %v", err)
		}
		afterSecond := snapshotTree(t, sessionDir)
		assertSnapshotEqual(t, afterFirst, afterSecond)
	})

	t.Run("cleanup owns orphan status fields and pid removal", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "cleanup-home")
		sessionDir := filepath.Join(home, "sessions", "python-running-cleanup")
		createPhase13Session(t, sessionDir, "python-running-cleanup", map[string]any{
			"status":              "running",
			"python_unknown_meta": "preserve-me",
		}, phase13Transcript("Codex", "Claude"))
		if err := os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte("0"), 0o644); err != nil {
			t.Fatalf("write pid: %v", err)
		}

		before := snapshotTree(t, home)
		report, err := runner.CleanupSessions(home, 500, false)
		if err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if report["orphaned_count"] != 1 {
			t.Fatalf("orphaned_count = %v, want 1", report["orphaned_count"])
		}
		afterFirst := snapshotTree(t, home)
		assertChangedFiles(t, before, afterFirst, []string{
			"sessions/python-running-cleanup/meta.json",
			"sessions/python-running-cleanup/relay.pid",
		})
		meta := readJSONObjectFile(t, filepath.Join(sessionDir, "meta.json"))
		if meta["status"] != "orphaned" || meta["python_unknown_meta"] != "preserve-me" {
			t.Fatalf("cleanup meta = %#v", meta)
		}
		if _, ok := meta["orphaned_at"].(string); !ok {
			t.Fatalf("cleanup did not write orphaned_at: %#v", meta)
		}

		secondReport, err := runner.CleanupSessions(home, 500, false)
		if err != nil {
			t.Fatalf("cleanup second pass: %v", err)
		}
		if secondReport["orphaned_count"] != 0 {
			t.Fatalf("second orphaned_count = %v, want 0", secondReport["orphaned_count"])
		}
		afterSecond := snapshotTree(t, home)
		assertSnapshotEqual(t, afterFirst, afterSecond)
	})

	t.Run("steering is explicitly append-only and non-idempotent", func(t *testing.T) {
		sessionDir := filepath.Join(t.TempDir(), "python-steering")
		createPhase13Session(t, sessionDir, "python-steering", map[string]any{
			"status":              "running",
			"python_unknown_meta": "preserve-me",
		}, phase13Transcript("Codex", "Gemini"))

		before := snapshotTree(t, sessionDir)
		if _, err := runner.QueueSteeringPrompt(sessionDir, "first steer"); err != nil {
			t.Fatalf("queue steering: %v", err)
		}
		afterFirst := snapshotTree(t, sessionDir)
		assertChangedFiles(t, before, afterFirst, []string{"steering.json"})
		if _, err := runner.QueueSteeringPrompt(sessionDir, "second steer"); err != nil {
			t.Fatalf("queue second steering: %v", err)
		}
		afterSecond := snapshotTree(t, sessionDir)
		if snapshotsEqual(afterFirst, afterSecond) {
			t.Fatalf("steering second pass was unexpectedly idempotent")
		}
		meta := readJSONObjectFile(t, filepath.Join(sessionDir, "meta.json"))
		if meta["python_unknown_meta"] != "preserve-me" {
			t.Fatalf("steering changed unknown meta: %#v", meta)
		}
		steering := readJSONArrayFile(t, filepath.Join(sessionDir, "steering.json"))
		if len(steering) != 2 {
			t.Fatalf("steering entries = %d, want 2: %#v", len(steering), steering)
		}
	})
}

func buildPhase13Corpus(t *testing.T, home string) []corpusSession {
	t.Helper()
	sessionsDir := filepath.Join(home, "sessions")
	corpus := []corpusSession{}
	add := func(name string, dir string) {
		corpus = append(corpus, corpusSession{name: name, dir: dir})
	}

	add("python-codex-only", createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-codex-only"),
		"python-codex-only",
		map[string]any{
			"slots": []any{
				slotMeta("codex", "slot_0", map[string]any{"thread_id": "codex-a-python"}),
				slotMeta("codex", "slot_1", map[string]any{"thread_id": "codex-b-python"}),
			},
			"python_unknown_meta": "codex legacy",
		},
		phase13Transcript("Codex (A)", "Codex (B)"),
	))
	add("python-claude-only", createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-claude-only"),
		"python-claude-only",
		map[string]any{
			"slots": []any{
				slotMeta("claude", "slot_0", map[string]any{"session_id": "claude-a-python", "cwd": "/repo"}),
				slotMeta("claude", "slot_1", map[string]any{"session_id": "claude-b-python", "cwd": "/repo"}),
			},
			"provider_result": map[string]any{"source": "python-jsonl"},
		},
		phase13Transcript("Claude Code (A)", "Claude Code (B)"),
	))
	add("python-gemini-only", createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-gemini-only"),
		"python-gemini-only",
		map[string]any{
			"slots": []any{
				slotMeta("gemini", "slot_0", map[string]any{"session_ref": "gemini-a-python"}),
				slotMeta("gemini", "slot_1", map[string]any{"session_ref": "gemini-b-python"}),
			},
		},
		phase13Transcript("Gemini (A)", "Gemini (B)"),
	))
	add("python-mixed-providers", createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-mixed-providers"),
		"python-mixed-providers",
		map[string]any{
			"slots": []any{
				slotMeta("codex", "slot_0", map[string]any{"thread_id": "codex-python"}),
				slotMeta("claude", "slot_1", map[string]any{"session_id": "claude-python", "cwd": "/repo"}),
			},
			"facilitator_backend": "gemini",
		},
		phase13Transcript("Codex", "Claude Code"),
	))

	relayBackendDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-relay-backend"),
		"python-relay-backend",
		map[string]any{
			"slots": []any{
				slotMeta("relay", "slot_0", map[string]any{"recipe_id": "review-panel", "child_session_id": "relay-child-python"}),
				slotMeta("codex", "slot_1", map[string]any{"thread_id": "codex-python"}),
			},
			"backend_profiles": map[string]any{"review-panel": map[string]any{"backend": "relay"}},
			"relay_recipes":    map[string]any{"review-panel": map[string]any{"max_rounds": 2}},
		},
		phase13Transcript("Relay", "Codex"),
	)
	appendRelayBackendEvents(t, store.New(relayBackendDir), "python-relay-backend")
	add("python-relay-backend", relayBackendDir)

	nestedDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-nested-relay-profiles"),
		"python-nested-relay-profiles",
		map[string]any{
			"slots": []any{
				slotMeta("relay", "slot_0", map[string]any{"recipe_id": "outer-review", "composition_path": "root.slot_0"}),
				slotMeta("relay", "slot_1", map[string]any{"recipe_id": "inner-review", "composition_path": "root.slot_1"}),
			},
			"backend_profiles": map[string]any{
				"outer-review": map[string]any{"backend": "relay", "recipe": "review-panel"},
				"inner-review": map[string]any{"backend": "relay", "recipe": "one-pass-review"},
			},
		},
		phase13Transcript("Relay (A)", "Relay (B)"),
	)
	appendRelayBackendEvents(t, store.New(nestedDir), "python-nested-relay-profiles")
	add("python-nested-relay-profiles", nestedDir)

	dynamicDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-dynamic-child-relay"),
		"python-dynamic-child-relay",
		map[string]any{
			"dynamic_mode": "ask",
			"slots": []any{
				slotMeta("codex", "slot_0", map[string]any{"thread_id": "codex-python"}),
				slotMeta("gemini", "slot_1", map[string]any{"session_ref": "gemini-python"}),
			},
		},
		phase13Transcript("Codex", "Gemini"),
	)
	appendDynamicChildEvents(t, store.New(dynamicDir), "python-dynamic-child-relay")
	add("python-dynamic-child-relay", dynamicDir)

	steeringResumeDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-steering-resume"),
		"python-steering-resume",
		map[string]any{
			"status":       "interrupted",
			"resumed_from": "python-parent-session",
			"stop_reason":  "interrupted",
		},
		phase13Transcript("Codex", "Claude Code"),
	)
	writeJSONFile(t, filepath.Join(steeringResumeDir, "steering.json"), []any{
		map[string]any{"id": "legacy-steer", "prompt": "continue with failures", "source": "steer", "created_at": phase13Timestamp},
	})
	add("python-steering-resume", steeringResumeDir)

	stopKillDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-stop-kill-cleanup"),
		"python-stop-kill-cleanup",
		map[string]any{
			"status":      "killed",
			"stop_reason": "killed",
			"killed_at":   phase13Timestamp,
		},
		phase13Transcript("Claude Code", "Gemini"),
	)
	add("python-stop-kill-cleanup", stopKillDir)

	displayDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-display-export"),
		"python-display-export",
		map[string]any{"display_exported_at": phase13Timestamp},
		phase13Transcript("Codex", "Claude Code"),
	)
	if err := os.WriteFile(filepath.Join(displayDir, "transcript.html"), []byte("<html>python-era display</html>"), 0o644); err != nil {
		t.Fatalf("write display html: %v", err)
	}
	add("python-display-export", displayDir)

	graphRepairDir := createPhase13Session(
		t,
		filepath.Join(sessionsDir, "python-graph-repair"),
		"python-graph-repair",
		map[string]any{},
		phase13Transcript("Codex", "Gemini"),
	)
	appendRootEvents(t, store.New(graphRepairDir), "python-graph-repair")
	if err := store.New(graphRepairDir).SaveGraph(map[string]any{
		"nodes": map[string]any{"stale": map[string]any{"status": "stale"}},
		"edges": []any{},
	}); err != nil {
		t.Fatalf("save graph repair fixture: %v", err)
	}
	add("python-graph-repair", graphRepairDir)

	contractDir := buildContractFixtureSession(t, filepath.Join(sessionsDir, "python-contract-fixture"))
	add("python-contract-fixture", contractDir)

	goFixtureDir := filepath.Join(sessionsDir, "go-created-phase4")
	copyDir(t, filepath.Join(repoRoot(t), "testdata", "sessions", "go-created-phase4"), goFixtureDir)
	ensureDisplayable(t, goFixtureDir, "go-created-phase4")
	add("go-created-phase4", goFixtureDir)

	return corpus
}

func createPhase13Session(t *testing.T, sessionDir string, sessionID string, patch map[string]any, transcript []any) string {
	t.Helper()
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	meta := map[string]any{
		"session_id":       sessionID,
		"task":             "Phase 13 corpus " + sessionID,
		"title":            "Phase 13 corpus " + sessionID,
		"status":           "completed",
		"mode":             "adversarial",
		"dynamic_mode":     "off",
		"created_at":       phase13Timestamp,
		"updated_at":       phase13Timestamp,
		"actual_rounds":    len(transcript),
		"max_rounds":       len(transcript),
		"round_limit_mode": "fixed",
		"ledger":           ledger([]string{"common ground"}, nil, nil),
		"slots": []any{
			slotMeta("codex", "slot_0", map[string]any{"thread_id": sessionID + "-a"}),
			slotMeta("codex", "slot_1", map[string]any{"thread_id": sessionID + "-b"}),
		},
	}
	for key, value := range patch {
		meta[key] = value
	}
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), meta)
	writeJSONFile(t, filepath.Join(sessionDir, "transcript.json"), transcript)
	return sessionDir
}

func ensureDisplayable(t *testing.T, sessionDir string, sessionID string) {
	t.Helper()
	meta := readJSONObjectFile(t, filepath.Join(sessionDir, "meta.json"))
	transcript := phase13Transcript("Codex", "Relay")
	meta["actual_rounds"] = len(transcript)
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), meta)
	writeJSONFile(t, filepath.Join(sessionDir, "transcript.json"), transcript)
}

func buildContractFixtureSession(t *testing.T, sessionDir string) string {
	t.Helper()
	fixtureRoot := filepath.Join(repoRoot(t), "testdata", "contracts")
	if err := os.MkdirAll(filepath.Join(sessionDir, "artifacts"), 0o755); err != nil {
		t.Fatalf("mkdir contract artifacts: %v", err)
	}
	copyDir(t, filepath.Join(fixtureRoot, "artifacts"), filepath.Join(sessionDir, "artifacts"))
	copyFile(t, filepath.Join(fixtureRoot, "artifact-index.json"), filepath.Join(sessionDir, "artifacts", "index.json"))
	writeCanonicalEvents(t, filepath.Join(fixtureRoot, "events-with-child-contracts.json"), filepath.Join(sessionDir, "events.jsonl"))
	createPhase13Session(t, sessionDir, filepath.Base(sessionDir), map[string]any{
		"slots": []any{
			slotMeta("relay", "slot_0", map[string]any{"recipe_id": "review-panel"}),
			slotMeta("codex", "slot_1", map[string]any{"thread_id": "codex-contract"}),
		},
	}, phase13Transcript("Relay", "Codex"))
	if _, _, err := graph.RepairAndSaveFromEvents(store.New(sessionDir)); err != nil {
		t.Fatalf("repair contract graph: %v", err)
	}
	return sessionDir
}

func appendRootEvents(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	if _, err := st.AppendSessionEventV1("node_started", graph.RootNodeID, "Root started", map[string]any{
		"session_ref": sessionID,
	}, store.EventOptions{EventID: sessionID + "-start", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append root start: %v", err)
	}
	if _, err := st.AppendSessionEventV1("node_completed", graph.RootNodeID, "Root completed", map[string]any{
		"session_ref":   sessionID,
		"stop_reason":   "fixed_rounds",
		"actual_rounds": 2,
	}, store.EventOptions{EventID: sessionID + "-complete", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append root complete: %v", err)
	}
}

func appendRelayBackendEvents(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	appendRootStartOnly(t, st, sessionID)
	if _, err := st.AppendSessionEventV1("relay_backend_child_completed", "relay_child_"+strings.ReplaceAll(sessionID, "-", "_"), "Relay backend child completed", map[string]any{
		"parent_node_id":   graph.RootNodeID,
		"slot_id":          "slot_0",
		"composition_path": "root.slot_0",
		"recipe_id":        "review-panel",
		"child_session_id": sessionID + "-child",
		"contract_refs":    map[string]any{},
	}, store.EventOptions{EventID: sessionID + "-relay-child", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append relay child: %v", err)
	}
	appendRootCompleteOnly(t, st, sessionID)
}

func appendDynamicChildEvents(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	appendRootStartOnly(t, st, sessionID)
	if _, err := st.AppendSessionEventV1("spawn_proposed", graph.RootNodeID, "Spawn proposed", map[string]any{
		"proposal_id":          "sp_phase13",
		"contested_lineage_id": "risk-lineage",
		"recipe_id":            "review-panel",
	}, store.EventOptions{EventID: sessionID + "-spawn-proposed", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append spawn proposed: %v", err)
	}
	if _, err := st.AppendSessionEventV1("spawn_admitted", graph.RootNodeID, "Spawn admitted", map[string]any{
		"proposal_id":   "sp_phase13",
		"child_node_id": "child_phase13",
	}, store.EventOptions{EventID: sessionID + "-spawn-admitted", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append spawn admitted: %v", err)
	}
	if _, err := st.AppendSessionEventV1("child_node_created", "child_phase13", "Child node created", map[string]any{
		"parent_node_id": graph.RootNodeID,
		"recipe_id":      "review-panel",
	}, store.EventOptions{EventID: sessionID + "-child-created", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append child created: %v", err)
	}
	if _, err := st.AppendSessionEventV1("child_session_completed", "child_phase13", "Child completed", map[string]any{
		"child_session_id": sessionID + "-child",
		"proposal_id":      "sp_phase13",
		"recipe_id":        "review-panel",
		"contract_refs":    map[string]any{},
	}, store.EventOptions{EventID: sessionID + "-child-complete", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append child complete: %v", err)
	}
	appendRootCompleteOnly(t, st, sessionID)
}

func appendRootStartOnly(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	if _, err := st.AppendSessionEventV1("node_started", graph.RootNodeID, "Root started", map[string]any{
		"session_ref": sessionID,
	}, store.EventOptions{EventID: sessionID + "-start", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append root start: %v", err)
	}
}

func appendRootCompleteOnly(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	if _, err := st.AppendSessionEventV1("node_completed", graph.RootNodeID, "Root completed", map[string]any{
		"session_ref": sessionID,
	}, store.EventOptions{EventID: sessionID + "-complete", Timestamp: phase13Timestamp}); err != nil {
		t.Fatalf("append root complete: %v", err)
	}
}

func phase13Transcript(first string, second string) []any {
	return []any{
		map[string]any{
			"round":   1,
			"from":    first,
			"content": "First corpus turn",
			"ledger":  ledger(nil, []string{"risk-a"}, nil),
		},
		map[string]any{
			"round":   2,
			"from":    second,
			"content": "Second corpus turn",
			"ledger":  ledger([]string{"risk-a resolved"}, nil, []string{"risk-a"}),
		},
	}
}

func slotMeta(backend string, slotID string, state map[string]any) map[string]any {
	label := map[string]string{
		"claude": "Claude Code",
		"codex":  "Codex",
		"gemini": "Gemini",
		"relay":  "Relay",
	}[backend]
	return map[string]any{
		"backend": backend,
		"slot_id": slotID,
		"label":   label,
		"state":   state,
	}
}

func ledger(settled []string, contested []string, withdrawn []string) map[string]any {
	return map[string]any{
		"settled":   anyStrings(settled),
		"contested": anyStrings(contested),
		"withdrawn": anyStrings(withdrawn),
	}
}

func anyStrings(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

func snapshotTree(t *testing.T, root string) treeSnapshot {
	t.Helper()
	snapshot := treeSnapshot{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(rel)] = data
		return nil
	}); err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func assertSnapshotEqual(t *testing.T, before treeSnapshot, after treeSnapshot) {
	t.Helper()
	if !snapshotsEqual(before, after) {
		t.Fatalf("tree changed unexpectedly: %v", changedFiles(before, after))
	}
}

func snapshotsEqual(left treeSnapshot, right treeSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for path, leftData := range left {
		rightData, ok := right[path]
		if !ok || !bytes.Equal(leftData, rightData) {
			return false
		}
	}
	return true
}

func assertChangedFiles(t *testing.T, before treeSnapshot, after treeSnapshot, want []string) {
	t.Helper()
	got := changedFiles(before, after)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changed files = %v, want %v", got, want)
	}
}

func changedFiles(before treeSnapshot, after treeSnapshot) []string {
	seen := map[string]bool{}
	for path := range before {
		seen[path] = true
	}
	for path := range after {
		seen[path] = true
	}
	files := make([]string, 0, len(seen))
	for path := range seen {
		beforeData, beforeOK := before[path]
		afterData, afterOK := after[path]
		if beforeOK != afterOK || !bytes.Equal(beforeData, afterData) {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files
}

func readJSONObjectFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	value, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func readJSONArrayFile(t *testing.T, path string) []any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	value, err := contracts.DecodeJSONBytes(data)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s must contain a JSON array", path)
	}
	return items
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeCanonicalEvents(t *testing.T, sourcePath string, targetPath string) {
	t.Helper()
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	value, err := contracts.DecodeJSONBytes(data)
	if err != nil {
		t.Fatalf("decode %s: %v", sourcePath, err)
	}
	rawEvents, ok := value.([]any)
	if !ok {
		t.Fatalf("%s must contain a JSON array", sourcePath)
	}
	lines := make([][]byte, 0, len(rawEvents))
	for _, rawEvent := range rawEvents {
		event, err := contracts.ValidateSessionEvent(rawEvent)
		if err != nil {
			t.Fatalf("validate fixture event: %v", err)
		}
		line, err := contracts.CanonicalJSONBytes(event)
		if err != nil {
			t.Fatalf("canonical fixture event: %v", err)
		}
		lines = append(lines, line)
	}
	body := bytes.Join(lines, []byte("\n"))
	body = append(body, '\n')
	if err := os.WriteFile(targetPath, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", targetPath, err)
	}
}

func copyDir(t *testing.T, source string, target string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}
		copyFile(t, path, targetPath)
		return nil
	}); err != nil {
		t.Fatalf("copy dir %s: %v", source, err)
	}
}

func copyFile(t *testing.T, source string, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s does not contain go.mod: %v", root, err)
	}
	return root
}

func TestPhase13CorpusCoversRequiredSessionShapes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "relay-home")
	corpus := buildPhase13Corpus(t, home)
	got := map[string]bool{}
	for _, session := range corpus {
		got[session.name] = true
	}
	want := []string{
		"python-codex-only",
		"python-claude-only",
		"python-gemini-only",
		"python-mixed-providers",
		"python-relay-backend",
		"python-nested-relay-profiles",
		"python-dynamic-child-relay",
		"python-steering-resume",
		"python-stop-kill-cleanup",
		"python-display-export",
		"python-contract-fixture",
		"python-graph-repair",
		"go-created-phase4",
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("missing corpus session %q from %v", name, sortedCorpusNames(corpus))
		}
	}
}

func sortedCorpusNames(corpus []corpusSession) []string {
	names := make([]string, 0, len(corpus))
	for _, session := range corpus {
		names = append(names, session.name)
	}
	sort.Strings(names)
	return names
}

func TestPhase13SnapshotDiffIncludesAddedChangedAndRemovedFiles(t *testing.T) {
	before := treeSnapshot{
		"changed.txt": []byte("old"),
		"removed.txt": []byte("gone"),
	}
	after := treeSnapshot{
		"added.txt":   []byte("new"),
		"changed.txt": []byte("new"),
	}
	got := changedFiles(before, after)
	want := []string{"added.txt", "changed.txt", "removed.txt"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("changed files = %v, want %v", got, want)
	}
}
