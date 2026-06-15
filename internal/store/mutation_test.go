package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestSaveContractArtifactPreservesHistoricalDuplicateRefIDs(t *testing.T) {
	st := New(t.TempDir())
	firstPayload := map[string]any{"kind": "recipe", "schema_version": 1, "id": "dup", "value": "first"}
	secondPayload := map[string]any{"kind": "recipe", "schema_version": 1, "id": "dup", "value": "second"}

	firstRef, err := st.SaveContractArtifact("recipes", "dup", firstPayload, "recipe:dup")
	if err != nil {
		t.Fatalf("save first artifact: %v", err)
	}
	secondRef, err := st.SaveContractArtifact("recipes", "dup", secondPayload, "recipe:dup")
	if err != nil {
		t.Fatalf("save second artifact: %v", err)
	}
	if firstRef["digest"] == secondRef["digest"] {
		t.Fatalf("test setup produced identical digests")
	}

	index := st.ArtifactIndex()
	entries := index["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("artifact index entries = %d, want 2: %#v", len(entries), entries)
	}
	if _, err := st.ResolveArtifactRef("recipe:dup", ""); err == nil {
		t.Fatalf("ResolveArtifactRef without digest unexpectedly succeeded")
	}
	resolved, err := st.ResolveArtifactRef("recipe:dup", secondRef["digest"].(string))
	if err != nil {
		t.Fatalf("resolve second ref: %v", err)
	}
	payload, err := st.LoadArtifactPayloadRaw(resolved)
	if err != nil {
		t.Fatalf("load second payload: %v", err)
	}
	if payload["value"] != "second" {
		t.Fatalf("loaded payload value = %v, want second", payload["value"])
	}
}

func TestAtomicWriteFileCreatesParentsAndOverwrites(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "nested", "payload.json")

	if err := AtomicWriteFile(path, []byte(`{"value":"first"}`)); err != nil {
		t.Fatalf("atomic write first: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first write: %v", err)
	}
	if string(data) != `{"value":"first"}` {
		t.Fatalf("first write = %s", data)
	}

	if err := AtomicWriteFile(path, []byte(`{"value":"second"}`)); err != nil {
		t.Fatalf("atomic write overwrite: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overwrite: %v", err)
	}
	if string(data) != `{"value":"second"}` {
		t.Fatalf("overwrite = %s", data)
	}
}

func TestAtomicWriteFileRemovesTempFileOnRenameFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir target directory: %v", err)
	}

	if err := AtomicWriteFile(path, []byte("payload")); err == nil {
		t.Fatalf("atomic write unexpectedly succeeded over directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root entries: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".target.json.") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}

func TestAppendSessionEventWritesCanonicalStrictV1AndReplacesArtifactPathRefs(t *testing.T) {
	st := New(t.TempDir())
	noteRef, err := st.SaveArtifact("notes", "one", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("save artifact: %v", err)
	}
	event, err := st.AppendSessionEventV1(
		"note_written",
		"root",
		"Wrote note",
		map[string]any{"note_ref": "artifacts/notes/one.json"},
		EventOptions{EventID: "evt_note", Timestamp: "2026-05-19T00:00:00+00:00"},
	)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	payload := event["payload"].(map[string]any)
	if !contracts.IsArtifactRef(payload["note_ref"]) {
		t.Fatalf("note_ref was not replaced with artifact_ref: %#v", payload["note_ref"])
	}
	if payload["note_ref"].(map[string]any)["digest"] != noteRef["digest"] {
		t.Fatalf("note ref digest = %v, want %v", payload["note_ref"].(map[string]any)["digest"], noteRef["digest"])
	}
	validation, err := st.ValidateStrictV1("native_v1")
	if err != nil {
		t.Fatalf("validate strict v1: %v", err)
	}
	if validation["event_count"] != 1 {
		t.Fatalf("event count = %v, want 1", validation["event_count"])
	}

	rawLines := readEventLines(t, st.Root)
	canonical, err := contracts.CanonicalJSONBytes(event)
	if err != nil {
		t.Fatalf("canonical event: %v", err)
	}
	if !bytes.Equal(rawLines[0], canonical) {
		t.Fatalf("event line is not canonical:\n%s\n%s", rawLines[0], canonical)
	}
}

func TestSaveMetaTranscriptAndGraphArePythonReadableJSON(t *testing.T) {
	st := New(t.TempDir())
	if err := st.SaveMetaMap(map[string]any{"status": "completed", "task": "Task"}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	if err := st.SaveTranscriptItems([]any{}); err != nil {
		t.Fatalf("save transcript: %v", err)
	}
	if err := st.SaveGraph(map[string]any{"nodes": map[string]any{}, "edges": []any{}}); err != nil {
		t.Fatalf("save graph: %v", err)
	}
	for _, relPath := range []string{"meta.json", "transcript.json", "graph.json"} {
		data, err := os.ReadFile(filepath.Join(st.Root, relPath))
		if err != nil {
			t.Fatalf("read %s: %v", relPath, err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("%s is not JSON: %v", relPath, err)
		}
	}
}

func readEventLines(t *testing.T, root string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, EventsFilename))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) == 0 {
		t.Fatalf("no event lines")
	}
	return lines
}
