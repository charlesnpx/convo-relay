package format

import (
	"testing"

	"github.com/charlesnpx/convo-relay/internal/blobstore"
)

func TestBundleManifestRoundTripsBlobRef(t *testing.T) {
	rootPayload := []byte(`{"payload":"one"}`)
	rootPath := "payloads/root_session/session.json"
	manifest, err := BundleManifest(map[string]any{
		"convo_relay_version": "test",
		"terminal_status":     "completed",
		"stop_reason":         nil,
		"session_payload":     rootPath,
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory": []any{
			map[string]any{"kind": "diagnostics", "portable_id": "diagnostics", "path": "payloads/diagnostics/diagnostics.json", "blob": blobstore.RefForBytes([]byte(`{}`), "application/json")},
			map[string]any{"kind": "participant_transcript", "portable_id": "transcript", "path": "payloads/participant_transcript/transcript.json", "blob": blobstore.RefForBytes([]byte(`[]`), "application/json")},
			map[string]any{"kind": "root_session", "portable_id": "session", "path": rootPath, "blob": blobstore.RefForBytes(rootPayload, "application/json")},
		},
	})
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	encoded, err := CanonicalJSONBytes(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	decoded, err := DecodeStrictJSONObjectBytes(encoded)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if _, err := ValidateBundleManifest(decoded); err != nil {
		t.Fatalf("validate decoded manifest: %v", err)
	}
}
