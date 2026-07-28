package portable

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestVerifyDirectoryValidatesPortableExportClosure(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*testing.T, *portableVerifyFixture)
		wantErr          string
		wantPayloadCount int
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
		{
			name: "source identity stripped from source payload",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, "recipe", "artifact-000001", map[string]any{
					"kind":           "recipe",
					"schema_version": 1,
				}, portableTestSourceRef("recipe:neutral", 10)))
				fixture.stripEntrySource(t, "recipe", "artifact-000001")
				fixture.refresh(t)
			},
			wantErr: "requires source artifact identity",
		},
		{
			name: "coordinated root kind relabeling",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindExecutionWorkspace, "artifact-000001", map[string]any{
					"kind":           contracts.RootArtifactKindExecutionWorkspace,
					"schema_version": contracts.RootArtifactSchemaVersionV2,
					"digest_profile": contracts.DigestProfileV1,
				}, portableTestSourceRef("execution_workspace:selected", 11)))
				fixture.relabelPayload(t, contracts.RootArtifactKindExecutionWorkspace, "artifact-000001", contracts.RootArtifactKindRootRecipePlan, map[string]any{
					"kind":           contracts.RootArtifactKindRootRecipePlan,
					"schema_version": contracts.RootArtifactSchemaVersionV2,
					"digest_profile": contracts.DigestProfileV1,
				})
				fixture.refresh(t)
			},
			wantErr: "kind does not match source artifact kind",
		},
		{
			name: "malformed portable ref discriminator",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				fixture.replacePayload(t, "root_session", "session", map[string]any{
					"kind": "portable_root_session",
					"recipe_ref": map[string]any{
						"kind":        "not_portable_payload_ref",
						"portable_id": "artifact-000001",
					},
				})
				fixture.refresh(t)
			},
			wantErr: "requires kind portable_payload_ref",
		},
		{
			name: "duplicate exact source ref",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				source := portableTestSourceRef("recipe:neutral", 12)
				for _, id := range []string{"artifact-000001", "artifact-000002"} {
					fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, "recipe", id, map[string]any{
						"kind":           "recipe",
						"schema_version": 1,
					}, source))
				}
				fixture.refresh(t)
			},
			wantErr: "source artifact ref is duplicated",
		},
		{
			name: "multiple immutable source revisions",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				for index, id := range []string{"artifact-000001", "artifact-000002"} {
					fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindExecutionWorkspace, id, map[string]any{
						"kind":           contracts.RootArtifactKindExecutionWorkspace,
						"schema_version": contracts.RootArtifactSchemaVersionV2,
						"digest_profile": contracts.DigestProfileV1,
						"revision":       index + 1,
					}, portableTestSourceRef("execution_workspace:selected", 20+index)))
				}
				fixture.refresh(t)
			},
			wantPayloadCount: 5,
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
				wantCount := test.wantPayloadCount
				if wantCount == 0 {
					wantCount = 3
				}
				if report["status"] != "valid" || report["payload_count"] != wantCount {
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

func TestVerifyDirectoryValidatesPortableProviderLineageV2(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *portableVerifyFixture)
		wantErr string
	}{
		{name: "valid"},
		{
			name: "source identity mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue("provider_invocation", "artifact-000002").(map[string]any)["invocation"].(map[string]any)
				ref := invocation["provider_result_ref"].(map[string]any)
				ref["source_artifact_digest"] = tamperedDigest(ref["source_artifact_digest"].(string))
				fixture.replacePayload(t, "provider_invocation", "artifact-000002", map[string]any{
					"kind":           contracts.RootArtifactKindProviderInvocation,
					"schema_version": contracts.RootArtifactSchemaVersionV2,
					"digest_profile": contracts.DigestProfileV1,
					"invocation":     invocation,
				})
			},
			wantErr: "source identity mismatch",
		},
		{
			name: "result identity mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result := fixture.payloadValue("provider_result", "artifact-000001").(map[string]any)
				result["phase"] = "facilitator"
				fixture.replacePayload(t, "provider_result", "artifact-000001", result)
			},
			wantErr: "does not match invocation draft",
		},
		{
			name: "invocation payload kind mismatch cannot skip lineage",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue("provider_invocation", "artifact-000002").(map[string]any)
				invocation["kind"] = "diagnostics"
				fixture.replacePayload(t, "provider_invocation", "artifact-000002", invocation)
			},
			wantErr: "kind does not match inventory kind",
		},
		{
			name: "result ref discriminator mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue("provider_invocation", "artifact-000002").(map[string]any)
				ref := invocation["invocation"].(map[string]any)["provider_result_ref"].(map[string]any)
				ref["kind"] = "artifact_ref"
				fixture.replacePayload(t, "provider_invocation", "artifact-000002", invocation)
			},
			wantErr: "requires kind portable_payload_ref",
		},
		{
			name: "result ref and target stripped source identity",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue("provider_invocation", "artifact-000002").(map[string]any)
				ref := invocation["invocation"].(map[string]any)["provider_result_ref"].(map[string]any)
				delete(ref, "source_artifact_id")
				delete(ref, "source_artifact_digest")
				fixture.replacePayload(t, "provider_invocation", "artifact-000002", invocation)
				fixture.stripEntrySource(t, "provider_result", "artifact-000001")
			},
			wantErr: "requires source artifact identity",
		},
		{
			name: "provider result value is not object",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result := fixture.payloadValue("provider_result", "artifact-000001").(map[string]any)
				result["provider_result"] = "completed"
				fixture.replacePayload(t, "provider_result", "artifact-000001", result)
			},
			wantErr: "provider result provider_result must be an object",
		},
		{
			name: "provider result backend mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result := fixture.payloadValue("provider_result", "artifact-000001").(map[string]any)
				result["provider_result"].(map[string]any)["backend"] = "claude"
				fixture.replacePayload(t, "provider_result", "artifact-000001", result)
			},
			wantErr: "provider result backend does not match wrapper backend",
		},
		{
			name: "invocation result target mismatch",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue("provider_invocation", "artifact-000002").(map[string]any)["invocation"].(map[string]any)
				ref := invocation["provider_result_ref"].(map[string]any)
				ref["portable_id"] = "artifact-000002"
				ref["source_artifact_id"] = "provider_invocation:000001"
				ref["source_artifact_digest"] = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
				fixture.replacePayload(t, "provider_invocation", "artifact-000002", map[string]any{
					"kind":           contracts.RootArtifactKindProviderInvocation,
					"schema_version": contracts.RootArtifactSchemaVersionV2,
					"digest_profile": contracts.DigestProfileV1,
					"invocation":     invocation,
				})
			},
			wantErr: "does not target provider_result",
		},
		{
			name: "orphan provider result",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result, _ := portableProviderAttemptPayloads(t, fixture, 2, "artifact-000003", "artifact-000004", 2)
				fixture.appendPayload(result)
			},
			wantErr: "is orphaned",
		},
		{
			name: "duplicate provider result correlation",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result, _ := portableProviderAttemptPayloads(t, fixture, 1, "artifact-000003", "artifact-000004", 2)
				fixture.appendPayload(result)
			},
			wantErr: "duplicate invocation_id and runner_attempt",
		},
		{
			name: "duplicate provider invocation correlation",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				_, invocation := portableProviderAttemptPayloads(t, fixture, 1, "artifact-000003", "artifact-000004", 2)
				value, err := contracts.DecodeStrictJSONObjectBytes(invocation.body)
				if err != nil {
					t.Fatalf("decode duplicate invocation: %v", err)
				}
				record := value["invocation"].(map[string]any)
				record["provider_launch_attempted"] = false
				record["provider_result_ref"] = nil
				fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindProviderInvocation, "artifact-000004", value, portableTestSourceRef("provider_invocation:000002", 24)))
			},
			wantErr: "duplicate invocation_id and runner_attempt",
		},
		{
			name: "shared provider result",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				invocation := fixture.payloadValue(contracts.RootArtifactKindProviderInvocation, "artifact-000002")
				fixture.appendPayload(mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindProviderInvocation, "artifact-000003", invocation, portableTestSourceRef("provider_invocation:000002", 25)))
			},
			wantErr: "multiple incoming invocation edges",
		},
		{
			name: "attempt two only is valid",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				fixture.setProviderAttempt(t, 2)
			},
		},
		{
			name: "gapped attempts are valid",
			mutate: func(t *testing.T, fixture *portableVerifyFixture) {
				result, invocation := portableProviderAttemptPayloads(t, fixture, 3, "artifact-000003", "artifact-000004", 3)
				fixture.appendPayload(result)
				fixture.appendPayload(invocation)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPortableProviderLineageFixture(t)
			if test.mutate != nil {
				test.mutate(t, fixture)
				fixture.refresh(t)
			}
			report, err := VerifyDirectory(fixture.dir)
			if test.wantErr == "" {
				if err != nil || report["schema_version"] != contracts.PortableExportV2 {
					t.Fatalf("verify provider lineage = %#v, %v", report, err)
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

func newPortableProviderLineageFixture(t *testing.T) *portableVerifyFixture {
	t.Helper()
	fixture := &portableVerifyFixture{dir: filepath.Join(t.TempDir(), "portable")}
	resultSource := map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             "provider_result:000001",
		"digest":         "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	invocationSource := map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             "provider_invocation:000001",
		"digest":         "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	}
	invocationDraft := map[string]any{
		"schema_version":            contracts.ProviderInvocationV2,
		"invocation_id":             "participant:000001",
		"phase":                     "participant",
		"actor":                     "Agent A",
		"participant_ordinal":       1,
		"backend":                   "codex",
		"mapped_working_directory":  ".",
		"runner_attempt":            1,
		"provider_launch_attempted": true,
		"provider_retry":            "allow",
		"started_at":                "2026-01-01T00:00:00Z",
		"completed_at":              "2026-01-01T00:00:01Z",
		"outcome":                   "completed",
		"failure_stage":             nil,
		"classification":            nil,
		"provider_result_ref":       nil,
	}
	resultPayload := map[string]any{
		"kind":           contracts.RootArtifactKindProviderResult,
		"schema_version": contracts.RootArtifactSchemaVersionV2,
		"digest_profile": contracts.DigestProfileV1,
		"invocation_id":  invocationDraft["invocation_id"],
		"phase":          invocationDraft["phase"],
		"actor":          invocationDraft["actor"],
		"runner_attempt": invocationDraft["runner_attempt"],
		"provider_retry": invocationDraft["provider_retry"],
		"backend":        invocationDraft["backend"],
		"started_at":     invocationDraft["started_at"],
		"completed_at":   invocationDraft["completed_at"],
		"outcome":        invocationDraft["outcome"],
		"failure_stage":  invocationDraft["failure_stage"],
		"classification": invocationDraft["classification"],
		"provider_result": map[string]any{
			"backend":     "codex",
			"return_code": 0,
		},
		"invocation": invocationDraft,
	}
	boundInvocation := contracts.Materialize(invocationDraft).(map[string]any)
	boundInvocation["provider_result_ref"] = map[string]any{
		"kind":                   "portable_payload_ref",
		"portable_id":            "artifact-000001",
		"source_artifact_id":     resultSource["id"],
		"source_artifact_digest": resultSource["digest"],
	}
	invocationPayload := map[string]any{
		"kind":           contracts.RootArtifactKindProviderInvocation,
		"schema_version": contracts.RootArtifactSchemaVersionV2,
		"digest_profile": contracts.DigestProfileV1,
		"invocation":     boundInvocation,
	}
	fixture.payloads = []exportPayload{
		mustPortableVerifyPayload(t, "root_session", "session", map[string]any{
			"kind":            "portable_root_session",
			"terminal_status": "completed",
		}),
		mustPortableVerifyPayload(t, "participant_transcript", "transcript", []any{}),
		mustPortableVerifyPayload(t, "diagnostics", "diagnostics", map[string]any{
			"execution_kind": "recipe",
			"status":         "completed",
		}),
		mustPortableVerifyPayloadWithSource(t, "provider_result", "artifact-000001", resultPayload, resultSource),
		mustPortableVerifyPayloadWithSource(t, "provider_invocation", "artifact-000002", invocationPayload, invocationSource),
	}
	sort.Slice(fixture.payloads, func(i, j int) bool {
		return stringValue(fixture.payloads[i].entry["path"]) < stringValue(fixture.payloads[j].entry["path"])
	})
	fixture.refresh(t)
	return fixture
}

func mustPortableVerifyPayload(t *testing.T, kind string, id string, value any) exportPayload {
	t.Helper()
	payload, err := newExportPayload(kind, id, value, nil)
	if err != nil {
		t.Fatalf("build payload %s/%s: %v", kind, id, err)
	}
	return payload
}

func mustPortableVerifyPayloadWithSource(t *testing.T, kind string, id string, value any, sourceRef map[string]any) exportPayload {
	t.Helper()
	payload, err := newExportPayload(kind, id, value, sourceRef)
	if err != nil {
		t.Fatalf("build payload %s/%s: %v", kind, id, err)
	}
	return payload
}

func (f *portableVerifyFixture) payloadValue(kind string, id string) any {
	for _, payload := range f.payloads {
		if payload.entry["kind"] == kind && payload.entry["portable_id"] == id {
			value, err := contracts.DecodeStrictJSONBytes(payload.body)
			if err != nil {
				panic(err)
			}
			return value
		}
	}
	return nil
}

func (f *portableVerifyFixture) replacePayload(t *testing.T, kind string, id string, value any) {
	t.Helper()
	for index, payload := range f.payloads {
		if payload.entry["kind"] == kind && payload.entry["portable_id"] == id {
			var sourceRef map[string]any
			if payload.entry["source_artifact_id"] != nil {
				sourceRef = map[string]any{
					"id":     payload.entry["source_artifact_id"],
					"digest": payload.entry["source_artifact_digest"],
				}
			}
			f.payloads[index] = mustPortableVerifyPayloadWithSource(t, kind, id, value, sourceRef)
			return
		}
	}
	t.Fatalf("payload %s/%s not found", kind, id)
}

func (f *portableVerifyFixture) appendPayload(payload exportPayload) {
	f.payloads = append(f.payloads, payload)
	sort.Slice(f.payloads, func(i, j int) bool {
		return stringValue(f.payloads[i].entry["path"]) < stringValue(f.payloads[j].entry["path"])
	})
}

func (f *portableVerifyFixture) relabelPayload(t *testing.T, kind string, id string, newKind string, value any) {
	t.Helper()
	for index, payload := range f.payloads {
		if payload.entry["kind"] != kind || payload.entry["portable_id"] != id {
			continue
		}
		oldPath := filepath.Join(f.dir, filepath.FromSlash(stringValue(payload.entry["path"])))
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove relabeled payload: %v", err)
		}
		source := map[string]any{
			"id":     payload.entry["source_artifact_id"],
			"digest": payload.entry["source_artifact_digest"],
		}
		f.payloads[index] = mustPortableVerifyPayloadWithSource(t, newKind, id, value, source)
		sort.Slice(f.payloads, func(i, j int) bool {
			return stringValue(f.payloads[i].entry["path"]) < stringValue(f.payloads[j].entry["path"])
		})
		return
	}
	t.Fatalf("payload %s/%s not found", kind, id)
}

func (f *portableVerifyFixture) setProviderAttempt(t *testing.T, runnerAttempt int) {
	t.Helper()
	result := f.payloadValue(contracts.RootArtifactKindProviderResult, "artifact-000001").(map[string]any)
	result["runner_attempt"] = runnerAttempt
	result["invocation"].(map[string]any)["runner_attempt"] = runnerAttempt
	f.replacePayload(t, contracts.RootArtifactKindProviderResult, "artifact-000001", result)

	invocation := f.payloadValue(contracts.RootArtifactKindProviderInvocation, "artifact-000002").(map[string]any)
	invocation["invocation"].(map[string]any)["runner_attempt"] = runnerAttempt
	f.replacePayload(t, contracts.RootArtifactKindProviderInvocation, "artifact-000002", invocation)
}

func (f *portableVerifyFixture) stripEntrySource(t *testing.T, kind string, id string) {
	t.Helper()
	for _, payload := range f.payloads {
		if payload.entry["kind"] == kind && payload.entry["portable_id"] == id {
			delete(payload.entry, "source_artifact_id")
			delete(payload.entry, "source_artifact_digest")
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

func portableTestSourceRef(id string, revision int) map[string]any {
	return map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             id,
		"digest":         fmt.Sprintf("sha256:%064x", revision),
	}
}

func portableProviderAttemptPayloads(
	t *testing.T,
	fixture *portableVerifyFixture,
	runnerAttempt int,
	resultPortableID string,
	invocationPortableID string,
	sourceOrdinal int,
) (exportPayload, exportPayload) {
	t.Helper()
	resultSource := portableTestSourceRef(fmt.Sprintf("provider_result:%06d", sourceOrdinal), sourceOrdinal*2+1)
	invocationSource := portableTestSourceRef(fmt.Sprintf("provider_invocation:%06d", sourceOrdinal), sourceOrdinal*2+2)

	result := fixture.payloadValue(contracts.RootArtifactKindProviderResult, "artifact-000001").(map[string]any)
	result["runner_attempt"] = runnerAttempt
	result["invocation"].(map[string]any)["runner_attempt"] = runnerAttempt

	invocation := fixture.payloadValue(contracts.RootArtifactKindProviderInvocation, "artifact-000002").(map[string]any)
	invocationRecord := invocation["invocation"].(map[string]any)
	invocationRecord["runner_attempt"] = runnerAttempt
	resultRef := invocationRecord["provider_result_ref"].(map[string]any)
	resultRef["portable_id"] = resultPortableID
	resultRef["source_artifact_id"] = resultSource["id"]
	resultRef["source_artifact_digest"] = resultSource["digest"]

	return mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindProviderResult, resultPortableID, result, resultSource),
		mustPortableVerifyPayloadWithSource(t, contracts.RootArtifactKindProviderInvocation, invocationPortableID, invocation, invocationSource)
}
