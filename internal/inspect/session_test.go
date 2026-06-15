package inspect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/graph"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestBuildShowTranscriptReportFiltersAndFormats(t *testing.T) {
	sessionDir := writeTranscriptSession(t)

	report, err := BuildShowTranscriptReport(sessionDir, 2, "")
	if err != nil {
		t.Fatalf("build show report: %v", err)
	}
	transcript := report["transcript"].([]any)
	if len(transcript) != 2 {
		t.Fatalf("filtered transcript = %d, want 2", len(transcript))
	}
	summary := report["summary"].(map[string]any)
	if summary["round_filter"] != "2+" {
		t.Fatalf("round_filter = %#v", summary["round_filter"])
	}
	if summary["actual_rounds"] != 5 {
		t.Fatalf("actual_rounds = %#v, want persisted value 5", summary["actual_rounds"])
	}

	markdown := FormatTranscriptMarkdown(report)
	if strings.Contains(markdown, "Round 1 - Codex (A)") || !strings.Contains(markdown, "Round 2 - Codex (B)") {
		t.Fatalf("markdown did not honor filter:\n%s", markdown)
	}
	if !strings.Contains(markdown, "## Final Ledger") || !strings.Contains(markdown, "Settled (1): done") || !strings.Contains(markdown, "**Ledger**: settled 0 | contested 1 | withdrawn 1") {
		t.Fatalf("markdown missing ledger summary:\n%s", markdown)
	}

	if _, err := BuildShowTranscriptReport(sessionDir, 1, "2-3"); err == nil {
		t.Fatalf("expected mutually exclusive round filter error")
	}
}

func TestBuildExportReportMarksIncompleteAndIncludesDiagnostics(t *testing.T) {
	sessionDir := writeTranscriptSession(t)
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), map[string]any{
		"session_id":       "phase7-session",
		"task":             "Phase 7 <unsafe> session",
		"title":            "Phase 7 <unsafe> session",
		"status":           "failed",
		"round_limit_mode": "fixed",
		"actual_rounds":    1,
		"max_rounds":       5,
		"slots":            []any{map[string]any{"backend": "codex"}, map[string]any{"backend": "codex"}},
		"ledger":           map[string]any{"settled": []any{}, "contested": []any{"auth"}, "withdrawn": []any{}},
	})
	st := store.New(sessionDir)
	if _, err := st.AppendSessionEventV1("provider_failure", graph.RootNodeID, "Provider failure", map[string]any{
		"phase":            "turn",
		"actor":            "Codex (A)",
		"backend":          "codex",
		"category":         "auth",
		"retryable":        false,
		"attempts":         1,
		"sanitized_detail": "Authentication error: token expired",
		"remediation_code": "codex_login",
	}, store.EventOptions{}); err != nil {
		t.Fatalf("append provider failure: %v", err)
	}

	report, err := BuildExportReport(sessionDir, true)
	if err != nil {
		t.Fatalf("build export: %v", err)
	}
	if report["incomplete"] != true {
		t.Fatalf("export incomplete = %#v", report["incomplete"])
	}
	diagnostics := report["diagnostics"].(map[string]any)
	if diagnostics["attention_required"] != true || len(asSlice(diagnostics["recent_failures"])) != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	markdown := FormatExportMarkdown(report)
	if !strings.Contains(markdown, "**Incomplete**: true") || !strings.Contains(markdown, "**Ledger**: settled 0 | contested 1 | withdrawn 0") || !strings.Contains(markdown, "Contested (1): auth") {
		t.Fatalf("export markdown missing status/ledger:\n%s", markdown)
	}
	showMarkdown := FormatTranscriptMarkdown(report)
	if !strings.Contains(showMarkdown, "**Attention required**: true") || !strings.Contains(showMarkdown, "Codex (A): Authentication error") {
		t.Fatalf("show markdown missing diagnostics:\n%s", showMarkdown)
	}
}

func TestShowAndExportReportsIncludePerRoundModes(t *testing.T) {
	sessionDir := t.TempDir()
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), map[string]any{
		"session_id":       "mixed-mode-session",
		"task":             "Inspect mixed modes",
		"title":            "Inspect mixed modes",
		"status":           "completed",
		"mode":             "steelman",
		"round_limit_mode": "fixed",
		"actual_rounds":    2,
		"max_rounds":       2,
		"slots":            []any{map[string]any{"backend": "codex"}, map[string]any{"backend": "codex"}},
		"ledger":           map[string]any{"settled": []any{}, "contested": []any{}, "withdrawn": []any{}},
		"mode_history": []any{
			map[string]any{"source": "run", "status": "applied", "applied_mode": "adversarial", "queued_round": 0, "applied_round": 1},
			map[string]any{"source": "resume", "status": "applied", "applied_mode": "steelman", "queued_round": 1, "applied_round": 2},
		},
	})
	writeJSONFile(t, filepath.Join(sessionDir, "transcript.json"), []any{
		map[string]any{"round": 1, "from": "Codex (A)", "content": "First turn"},
		map[string]any{"round": 2, "from": "Codex (B)", "mode": "steelman", "content": "Second turn"},
	})

	report, err := BuildShowTranscriptReport(sessionDir, 0, "")
	if err != nil {
		t.Fatalf("build show: %v", err)
	}
	transcript := report["transcript"].([]any)
	if transcript[0].(map[string]any)["mode"] != "adversarial" || transcript[1].(map[string]any)["mode"] != "steelman" {
		t.Fatalf("transcript modes = %#v", transcript)
	}
	summary := report["summary"].(map[string]any)
	if modes := summary["modes"].([]any); len(modes) != 2 || modes[0] != "adversarial" || modes[1] != "steelman" {
		t.Fatalf("summary modes = %#v", summary["modes"])
	}
	showMarkdown := FormatTranscriptMarkdown(report)
	if !strings.Contains(showMarkdown, "**Mode**: adversarial") || !strings.Contains(showMarkdown, "**Mode**: steelman") {
		t.Fatalf("show markdown missing per-round modes:\n%s", showMarkdown)
	}
	exportReport, err := BuildExportReport(sessionDir, false)
	if err != nil {
		t.Fatalf("build export: %v", err)
	}
	exportMarkdown := FormatExportMarkdown(exportReport)
	if !strings.Contains(exportMarkdown, "**Mode**: adversarial") || !strings.Contains(exportMarkdown, "**Mode**: steelman") {
		t.Fatalf("export markdown missing per-round modes:\n%s", exportMarkdown)
	}
}

func TestBuildHealthReports(t *testing.T) {
	global := BuildGlobalHealthReport(filepath.Join(t.TempDir(), "missing-settings.toml"))
	if global["scope"] != "global" || global["status"] != "ok" {
		t.Fatalf("global health = %#v", global)
	}

	sessionDir := writeTranscriptSession(t)
	report, err := BuildSessionHealthReport(sessionDir)
	if err != nil {
		t.Fatalf("session health: %v", err)
	}
	if report["scope"] != "session" || report["status"] != "ok" {
		t.Fatalf("session health = %#v", report)
	}
	formatted := FormatHealthReport(report)
	if !strings.Contains(formatted, "Health: ok") || !strings.Contains(formatted, "Scope: session") {
		t.Fatalf("formatted health:\n%s", formatted)
	}
}

func TestRenderDiffShowsLedgerTransitions(t *testing.T) {
	sessionDir := writeTranscriptSession(t)

	rendered, err := RenderDiff(sessionDir)
	if err != nil {
		t.Fatalf("render diff: %v", err)
	}
	for _, expected := range []string{
		"+ contested: alpha",
		"- contested: alpha",
		"+ withdrawn: alpha",
		"- withdrawn: alpha",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("diff missing %q:\n%s", expected, rendered)
		}
	}
}

func TestBuildDisplayHTMLEscapesTranscript(t *testing.T) {
	sessionDir := writeTranscriptSession(t)

	html, err := BuildDisplayHTML(sessionDir)
	if err != nil {
		t.Fatalf("build display html: %v", err)
	}
	if !strings.Contains(html, "Phase 7 &lt;unsafe&gt; session") || strings.Contains(html, "Phase 7 <unsafe> session") {
		t.Fatalf("html title was not escaped:\n%s", html)
	}
	if !strings.Contains(html, "Third &lt;unsafe&gt; turn") {
		t.Fatalf("html transcript was not escaped:\n%s", html)
	}

	emptyDir := t.TempDir()
	writeJSONFile(t, filepath.Join(emptyDir, "meta.json"), map[string]any{"title": "Empty"})
	writeJSONFile(t, filepath.Join(emptyDir, "transcript.json"), []any{})
	if _, err := BuildDisplayHTML(emptyDir); err == nil {
		t.Fatalf("expected empty transcript display error")
	}
}

func TestBuildDisplayHTMLRendersLaunchPromptAndPlan(t *testing.T) {
	sessionDir := t.TempDir()
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), map[string]any{
		"title":          "Display plan",
		"initial_prompt": "Review <unsafe> launch prompt",
		"actual_rounds":  1,
		"launch_plan": map[string]any{
			"explanation": "Validate <plan> rendering",
			"plan": []any{
				map[string]any{"step": "Persist launch plan", "status": "completed"},
				map[string]any{"step": "Render plan above transcript", "status": "in_progress"},
			},
		},
	})
	writeJSONFile(t, filepath.Join(sessionDir, "transcript.json"), []any{
		map[string]any{"round": 1, "from": "Codex", "content": "Transcript body"},
	})

	html, err := BuildDisplayHTML(sessionDir)
	if err != nil {
		t.Fatalf("build display html: %v", err)
	}
	for _, expected := range []string{
		"Launch Prompt",
		"Review &lt;unsafe&gt; launch prompt",
		"Launch Plan",
		"Final Ledger",
		"Ledger: settled 0 | contested 0 | withdrawn 0",
		"Validate &lt;plan&gt; rendering",
		"Persist launch plan",
		"status-completed",
		"status-in_progress",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("html missing %q:\n%s", expected, html)
		}
	}
	if strings.Index(html, "Launch Plan") > strings.Index(html, "Transcript body") {
		t.Fatalf("launch plan should render before transcript:\n%s", html)
	}
}

func TestBuildTraceReportLoadsGraphNodeTrace(t *testing.T) {
	sessionDir := t.TempDir()
	st := store.New(sessionDir)
	if err := st.EnsureSession(); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := st.SaveMetaMap(map[string]any{"session_id": "phase7-trace", "status": "completed"}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	traceRef, err := st.SaveArtifact("child_traces", "relay_backend_child_phase7", map[string]any{
		"kind":             "child_trace",
		"schema_version":   1,
		"child_node_id":    "relay_backend_child_phase7",
		"child_session_id": "phase7-child",
	})
	if err != nil {
		t.Fatalf("save trace: %v", err)
	}
	if _, err := st.AppendSessionEventV1("node_started", graph.RootNodeID, "Root started", map[string]any{"session_ref": "phase7-trace"}, store.EventOptions{}); err != nil {
		t.Fatalf("append root event: %v", err)
	}
	if _, err := st.AppendSessionEventV1("relay_backend_child_completed", "relay_backend_child_phase7", "Child completed", map[string]any{
		"parent_node_id":   graph.RootNodeID,
		"slot_id":          "slot_0",
		"composition_path": "root.slot_0",
		"recipe_id":        "phase7-child-recipe",
		"child_session_id": "phase7-child",
		"trace_ref":        traceRef,
	}, store.EventOptions{}); err != nil {
		t.Fatalf("append child event: %v", err)
	}
	if _, _, err := graph.RepairAndSaveFromEvents(st); err != nil {
		t.Fatalf("repair graph: %v", err)
	}

	trace, err := BuildTraceReport(sessionDir, "relay_backend_child_phase7")
	if err != nil {
		t.Fatalf("build trace report: %v", err)
	}
	if trace["child_session_id"] != "phase7-child" {
		t.Fatalf("trace payload = %#v", trace)
	}
}

func writeTranscriptSession(t *testing.T) string {
	t.Helper()
	sessionDir := t.TempDir()
	writeJSONFile(t, filepath.Join(sessionDir, "meta.json"), map[string]any{
		"session_id":       "phase7-inspect",
		"task":             "Inspect transcript behavior",
		"title":            "Phase 7 <unsafe> session",
		"status":           "completed",
		"mode":             "adversarial",
		"round_limit_mode": "fixed",
		"actual_rounds":    5,
		"max_rounds":       5,
		"elapsed_seconds":  1.25,
		"slots": []any{
			map[string]any{"backend": "codex"},
			map[string]any{"backend": "codex"},
		},
		"ledger": map[string]any{
			"settled":   []any{"done"},
			"contested": []any{},
			"withdrawn": []any{},
		},
	})
	writeJSONFile(t, filepath.Join(sessionDir, "transcript.json"), []any{
		map[string]any{
			"round":   1,
			"from":    "Codex (A)",
			"content": "First turn",
			"ledger": map[string]any{
				"settled":   []any{},
				"contested": []any{"alpha"},
				"withdrawn": []any{},
			},
		},
		map[string]any{
			"round":   2,
			"from":    "Codex (B)",
			"content": "Second turn",
			"ledger": map[string]any{
				"settled":   []any{},
				"contested": []any{"beta"},
				"withdrawn": []any{"alpha"},
			},
		},
		map[string]any{
			"round":   3,
			"from":    "Codex (A)",
			"content": "Third <unsafe> turn",
			"ledger": map[string]any{
				"settled":   []any{"done"},
				"contested": []any{},
				"withdrawn": []any{},
			},
		},
	})
	return sessionDir
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
