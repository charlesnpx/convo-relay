package portable

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestVerifyDirectoryValidatesPortableExportClosure(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *portableVerifyFixture)
		wantErr string
	}{
		{name: "valid"},
		{
			name: "manifest digest tamper",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				fixture.manifest["manifest_digest"] = tamperedDigest(fixture.manifest["manifest_digest"].(string))
				writePortableVerifyManifest(t, fixture)
			},
			wantErr: "portable export manifest digest mismatch",
		},
		{
			name: "declared payload size mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				entry := fixture.payloads[0].entry
				entry["size_bytes"] = entry["size_bytes"].(int) + 1
				fixture.refreshManifest(t)
			},
			wantErr: "size or digest mismatch",
		},
		{
			name: "unexpected file",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				writePortableVerifyFile(t, fixture.dir, "unexpected.json", []byte("{}"))
			},
			wantErr: "unexpected file",
		},
		{
			name: "unexpected directory",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				if err := os.Mkdir(filepath.Join(fixture.dir, "unexpected"), 0o755); err != nil {
					t.Fatalf("mkdir unexpected directory: %v", err)
				}
			},
			wantErr: "unexpected directory",
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				if err := os.Symlink("manifest.json", filepath.Join(fixture.dir, "manifest-link.json")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
			wantErr: "contains a symlink",
		},
		{
			name: "missing inventoried payload",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				if err := os.Remove(fixture.payloadPath("root_session", "session")); err != nil {
					t.Fatalf("remove inventoried payload: %v", err)
				}
			},
			wantErr: "missing or not a regular file",
		},
		{
			name: "source artifact ref retained",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				ref, err := contracts.ArtifactRefForPayload("recipe:source", map[string]any{
					"kind":           "recipe",
					"schema_version": 1,
					"id":             "source",
				})
				if err != nil {
					t.Fatalf("build source artifact ref: %v", err)
				}
				fixture.replacePayload(t, "root_session", "session", map[string]any{
					"kind":       "portable_root_session",
					"source_ref": ref,
				})
				fixture.refresh(t)
			},
			wantErr: "retains a source-session artifact ref",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPortableVerifyFixture(t)
			if test.mutate != nil {
				test.mutate(t, fixture)
			}
			report, err := VerifyDirectory(fixture.dir)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("verify valid export: %v", err)
				}
				if report["status"] != "valid" || report["payload_count"] != 3 {
					t.Fatalf("valid report = %#v", report)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verify error = %#v, %v; want %q", report, err, test.wantErr)
			}
		})
	}
}

type portableVerifyFixture struct {
	dir      string
	payloads []exportPayload
	manifest map[string]any
}

func newPortableVerifyFixture(t *testing.T) *portableVerifyFixture {
	t.Helper()
	fixture := &portableVerifyFixture{
		dir: filepath.Join(t.TempDir(), "portable"),
	}
	fixture.payloads = []exportPayload{
		mustPortableVerifyPayload(t, "root_session", "session", map[string]any{
			"kind":            "portable_root_session",
			"terminal_status": "completed",
		}),
		mustPortableVerifyPayload(t, "participant_transcript", "transcript", []any{
			map[string]any{"round": 1, "from": "A", "content": "done"},
		}),
		mustPortableVerifyPayload(t, "diagnostics", "diagnostics", map[string]any{
			"execution_kind": "recipe",
			"status":         "completed",
		}),
	}
	sort.Slice(fixture.payloads, func(i, j int) bool {
		return stringValue(fixture.payloads[i].entry["path"]) < stringValue(fixture.payloads[j].entry["path"])
	})
	fixture.refresh(t)
	return fixture
}

func mustPortableVerifyPayload(t *testing.T, kind string, id string, value any) exportPayload {
	t.Helper()
	payload, err := newExportPayload(kind, id, value, "")
	if err != nil {
		t.Fatalf("build payload %s/%s: %v", kind, id, err)
	}
	return payload
}

func (f *portableVerifyFixture) replacePayload(t *testing.T, kind string, id string, value any) {
	t.Helper()
	for index, payload := range f.payloads {
		if payload.entry["kind"] == kind && payload.entry["portable_id"] == id {
			f.payloads[index] = mustPortableVerifyPayload(t, kind, id, value)
			return
		}
	}
	t.Fatalf("payload %s/%s not found", kind, id)
}

func (f *portableVerifyFixture) refresh(t *testing.T) {
	t.Helper()
	for _, payload := range f.payloads {
		writePortableVerifyFile(t, f.dir, stringValue(payload.entry["path"]), payload.body)
	}
	f.refreshManifest(t)
}

func (f *portableVerifyFixture) refreshManifest(t *testing.T) {
	t.Helper()
	inventory := make([]any, 0, len(f.payloads))
	for _, payload := range f.payloads {
		inventory = append(inventory, payload.entry)
	}
	manifest, err := contracts.PortableExportManifest(map[string]any{
		"convo_relay_version": "test",
		"terminal_status":     "completed",
		"stop_reason":         nil,
		"session_payload":     "payloads/root_session/session.json",
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory":   inventory,
	})
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	f.manifest = manifest
	writePortableVerifyManifest(t, f)
}

func (f *portableVerifyFixture) payloadPath(kind string, id string) string {
	for _, payload := range f.payloads {
		if payload.entry["kind"] == kind && payload.entry["portable_id"] == id {
			return filepath.Join(f.dir, filepath.FromSlash(stringValue(payload.entry["path"])))
		}
	}
	return ""
}

func writePortableVerifyManifest(t *testing.T, fixture *portableVerifyFixture) {
	t.Helper()
	body, err := contracts.CanonicalJSONBytes(fixture.manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	writePortableVerifyFile(t, fixture.dir, "manifest.json", body)
}

func writePortableVerifyFile(t *testing.T, root string, relative string, body []byte) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(filename), err)
	}
	if err := os.WriteFile(filename, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func tamperedDigest(digest string) string {
	if strings.HasSuffix(digest, "0") {
		return digest[:len(digest)-1] + "1"
	}
	return digest[:len(digest)-1] + "0"
}
