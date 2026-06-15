package contracts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactIndexFixtureRefsMatchPayloadDigests(t *testing.T) {
	fixtures := fixtureRoot(t)
	index := mustValidateArtifactIndexFixture(t, filepath.Join(fixtures, "artifact-index.json"))
	entries := index["entries"].([]any)

	for _, rawEntry := range entries {
		entry := rawEntry.(map[string]any)
		ref := entry["ref"].(map[string]any)
		path := entry["path"].(string)
		payload := mustReadJSON(t, filepath.Join(fixtures, filepath.FromSlash(path)))
		digest, err := ContractDigest(payload)
		if err != nil {
			t.Fatalf("digest %s: %v", path, err)
		}
		if digest != ref["digest"] {
			t.Fatalf("%s digest mismatch: got %s want %s", path, digest, ref["digest"])
		}
	}
}

func TestMinimalEventLogFixtureIsStrictV1(t *testing.T) {
	fixtures := fixtureRoot(t)
	value := mustReadJSON(t, filepath.Join(fixtures, "minimal-event-log.json"))
	events, ok := value.([]any)
	if !ok {
		t.Fatalf("minimal-event-log.json must contain a JSON array")
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}

	event, err := ValidateSessionEvent(events[0])
	if err != nil {
		t.Fatalf("validate event: %v", err)
	}
	if event["event_id"] != "evt_fixture_root" {
		t.Fatalf("event_id = %v", event["event_id"])
	}
	canonical, err := CanonicalJSONBytes(event)
	if err != nil {
		t.Fatalf("canonical event: %v", err)
	}
	roundTrip, err := DecodeJSONObjectBytes(canonical)
	if err != nil {
		t.Fatalf("round trip canonical event: %v", err)
	}
	if _, err := ValidateSessionEvent(roundTrip); err != nil {
		t.Fatalf("round trip validate event: %v", err)
	}
}

func TestCanonicalJSONDoesNotHTMLEscapeStrings(t *testing.T) {
	text, err := CanonicalJSONText(map[string]any{"prompt": "<tag>&value"})
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if text != `{"prompt":"<tag>&value"}` {
		t.Fatalf("canonical JSON = %s", text)
	}
}

func TestCompiledPlanFixtureDigestIsPortable(t *testing.T) {
	fixtures := fixtureRoot(t)
	compiled := mustReadJSON(t, filepath.Join(fixtures, "compiled-plan.json"))
	digest, err := ContractDigest(compiled)
	if err != nil {
		t.Fatalf("compiled plan digest: %v", err)
	}

	index := mustValidateArtifactIndexFixture(t, filepath.Join(fixtures, "artifact-index.json"))
	for _, rawEntry := range index["entries"].([]any) {
		entry := rawEntry.(map[string]any)
		ref := entry["ref"].(map[string]any)
		if ref["id"] == "compiled_plan:8f0df39483ddee4d" && ref["digest"] != digest {
			t.Fatalf("compiled plan digest = %s, index wants %s", digest, ref["digest"])
		}
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

func mustReadJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	value, err := DecodeJSONBytes(data)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func mustValidateArtifactIndexFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	index, err := ValidateArtifactIndex(mustReadJSON(t, path))
	if err != nil {
		t.Fatalf("validate artifact index: %v", err)
	}
	return index
}
