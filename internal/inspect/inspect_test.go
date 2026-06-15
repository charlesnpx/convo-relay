package inspect

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestBuildContractsReportValidatesChildContractFixture(t *testing.T) {
	sessionDir := buildFixtureSession(t)
	report, err := BuildContractsReport(sessionDir, false, "", "")
	if err != nil {
		t.Fatalf("build report: %v", err)
	}

	validation := report["validation"].(map[string]any)
	if validation["ok"] != true {
		t.Fatalf("validation failed: %#v", validation)
	}
	if report["event_count"] != 2 {
		t.Fatalf("event_count = %v, want 2", report["event_count"])
	}
	if report["artifact_index_entry_count"] != 6 {
		t.Fatalf("artifact index entries = %v, want 6", report["artifact_index_entry_count"])
	}

	relayBundles := report["relay_backend_child_contract_bundles"].([]any)
	if len(relayBundles) != 1 {
		t.Fatalf("relay bundle count = %d, want 1", len(relayBundles))
	}
	relayBundle := relayBundles[0].(map[string]any)
	if _, ok := relayBundle["composition_path"]; !ok {
		t.Fatalf("relay bundle must include composition_path")
	}
	if relayBundle["child_session_id"] != "child-relay-fixture" {
		t.Fatalf("child_session_id = %v", relayBundle["child_session_id"])
	}
	assertRefOK(t, relayBundle, "child_result_ref")

	dynamicBundles := report["dynamic_child_contract_bundles"].([]any)
	if len(dynamicBundles) != 1 {
		t.Fatalf("dynamic bundle count = %d, want 1", len(dynamicBundles))
	}
	assertRefOK(t, dynamicBundles[0].(map[string]any), "child_invocation_ref")
}

func TestBuildContractsReportResolvesRefWithRawPayload(t *testing.T) {
	sessionDir := buildFixtureSession(t)
	report, err := BuildContractsReport(sessionDir, true, "recipe:review-panel", "")
	if err != nil {
		t.Fatalf("build report with ref: %v", err)
	}
	resolved := report["resolved_ref"].(map[string]any)
	if resolved["ok"] != true {
		t.Fatalf("resolved ref failed: %#v", resolved)
	}
	if resolved["kind"] != "recipe" {
		t.Fatalf("resolved kind = %v, want recipe", resolved["kind"])
	}
	payload := resolved["payload"].(map[string]any)
	if payload["id"] != "review-panel" {
		t.Fatalf("recipe payload id = %v", payload["id"])
	}
}

func TestBuildContractsReportReadsGoCreatedPhase4Fixture(t *testing.T) {
	report, err := BuildContractsReport(goCreatedPhase4Fixture(t), false, "", "")
	if err != nil {
		t.Fatalf("build report: %v", err)
	}

	validation := report["validation"].(map[string]any)
	if validation["ok"] != true {
		t.Fatalf("validation failed: %#v", validation)
	}
	if report["event_count"] != 3 {
		t.Fatalf("event_count = %v, want 3", report["event_count"])
	}
	if report["artifact_index_entry_count"] != 4 {
		t.Fatalf("artifact index entries = %v, want 4", report["artifact_index_entry_count"])
	}
	relayBundles := report["relay_backend_child_contract_bundles"].([]any)
	if len(relayBundles) != 1 {
		t.Fatalf("relay bundle count = %d, want 1", len(relayBundles))
	}
	relayBundle := relayBundles[0].(map[string]any)
	if relayBundle["composition_path"] != "root.slot_0" {
		t.Fatalf("composition_path = %v, want root.slot_0", relayBundle["composition_path"])
	}
	assertRefOK(t, relayBundle, "recipe_ref")
	assertRefOK(t, relayBundle, "compiled_plan_ref")
	assertRefOK(t, relayBundle, "child_invocation_ref")
	assertRefOK(t, relayBundle, "child_result_ref")
}

func assertRefOK(t *testing.T, bundle map[string]any, key string) {
	t.Helper()
	refs := bundle["refs"].(map[string]any)
	status := refs[key].(map[string]any)
	if status["ok"] != true {
		t.Fatalf("%s status failed: %#v", key, status)
	}
	if status["digest_valid"] != true {
		t.Fatalf("%s digest_valid = %v", key, status["digest_valid"])
	}
}

func buildFixtureSession(t *testing.T) string {
	t.Helper()
	fixtures := fixtureRoot(t)
	sessionDir := filepath.Join(t.TempDir(), "fixture-child-contracts")
	if err := os.MkdirAll(filepath.Join(sessionDir, "artifacts"), 0o755); err != nil {
		t.Fatalf("mkdir session artifacts: %v", err)
	}
	copyDir(t, filepath.Join(fixtures, "artifacts"), filepath.Join(sessionDir, "artifacts"))
	copyFile(t, filepath.Join(fixtures, "artifact-index.json"), filepath.Join(sessionDir, "artifacts", "index.json"))
	writeCanonicalEvents(t, filepath.Join(fixtures, "events-with-child-contracts.json"), filepath.Join(sessionDir, "events.jsonl"))
	return sessionDir
}

func writeCanonicalEvents(t *testing.T, sourcePath string, targetPath string) {
	t.Helper()
	value := mustReadJSON(t, sourcePath)
	events, ok := value.([]any)
	if !ok {
		t.Fatalf("%s must contain a JSON array", sourcePath)
	}
	var lines [][]byte
	for _, rawEvent := range events {
		event, err := contracts.ValidateSessionEvent(rawEvent)
		if err != nil {
			t.Fatalf("validate event: %v", err)
		}
		line, err := contracts.CanonicalJSONBytes(event)
		if err != nil {
			t.Fatalf("canonical event: %v", err)
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

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "contracts"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func goCreatedPhase4Fixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sessions", "go-created-phase4"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func mustReadJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	value, err := contracts.DecodeJSONBytes(data)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}
