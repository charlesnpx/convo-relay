package bundle

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/charlesnpx/convo-relay/v2/internal/semanticjson"
	"github.com/charlesnpx/convo-relay/v2/plan"
	"github.com/charlesnpx/convo-relay/v2/result"
)

func TestInvocationEvidencePresenceSurvivesBundle(t *testing.T) {
	value := verifyTestPlan()
	wantDigest, err := plan.Digest(value)
	if err != nil {
		t.Fatalf("digest test plan: %v", err)
	}
	for _, test := range []struct {
		name        string
		invocations *result.Count
		present     bool
	}{
		{name: "absent", invocations: nil, present: false},
		{name: "explicit zero", invocations: &result.Count{Count: 0}, present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := writeVerifyTestBundle(t, value, test.invocations)
			verified, err := VerifyPortableDirectory(directory, VerifyOptions{
				ExpectedPlanDigest: wantDigest,
				ExpectedSessionID:  value.SessionID,
			})
			if err != nil {
				t.Fatalf("verify bundle: %v", err)
			}
			if got := verified.Session.Root.Invocations != nil; got != test.present {
				t.Fatalf("invocation presence = %v, want %v", got, test.present)
			}
		})
	}
}

func verifyTestPlan() plan.Plan {
	return plan.Plan{
		Kind: plan.PlanKind, SchemaVersion: plan.SchemaVersion,
		SessionID: "bundle-presence", Provenance: plan.ProvenanceSupplied,
		Task: "verify invocation evidence", Timeouts: plan.Timeouts{TurnSeconds: 60, StallSeconds: 30},
		Mode: plan.ModeCooperative, Investigation: plan.InvestigationNormal,
		Actors:        []plan.Actor{{ID: "actor", Backend: plan.ActorBackendCodex}},
		Schedule:      plan.Schedule{Kind: plan.ScheduleSequence, Turns: 1, Order: []string{"actor"}},
		ProviderRetry: plan.ProviderRetry{Mode: plan.ProviderRetryForbid, MaxAttempts: 1},
		Workspace:     plan.Workspace{Mode: plan.WorkspaceModeCurrent},
		Inputs:        []plan.Input{}, Context: []plan.Input{}, Skills: []plan.Input{},
		ChildPolicy: plan.ChildPolicy{Mode: plan.ChildPolicyDeny, AllowedRecipes: []string{}},
		Result:      plan.Result{Source: plan.ResultSourceLastTurn, Format: plan.ResultFormatText},
	}
}

func writeVerifyTestBundle(t *testing.T, value plan.Plan, invocations *result.Count) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "bundle")
	root := SessionPayload{
		Plan: value, TerminalStatus: "completed", ResultSource: value.Result.Source,
		ValidationStatus: "pending", WorkspaceContentSource: "working_tree",
		Root: result.Root{
			ExecutionKind: "supplied", Status: "completed", Recipe: result.Recipe{},
			Turns:     result.Turns{Configured: 1, Completed: 0},
			Result:    result.RootResult{Source: value.Result.Source, ValidationStatus: "pending"},
			Workspace: result.Workspace{Mode: "current", WorkspaceContentSource: "working_tree"},
			Providers: map[string]string{}, ProviderRetry: value.ProviderRetry.Mode,
			Invocations: invocations, ReducerAttempts: result.Count{},
		},
	}
	transcript := []result.TranscriptEntry{}
	diagnostics := result.Diagnostics{AbandonedAttempts: []result.AbandonedAttempt{}, UnreferencedBlobs: []result.BlobDiagnostic{}}
	payloads := []struct {
		kind string
		id   string
		data any
	}{
		{kind: "diagnostics", id: "diagnostics", data: diagnostics},
		{kind: "participant_transcript", id: "transcript", data: transcript},
		{kind: "root_session", id: "session", data: root},
	}
	entries := make([]InventoryEntry, 0, len(payloads))
	bodies := make(map[string][]byte, len(payloads))
	for _, payload := range payloads {
		body, err := semanticjson.SemanticJSONBytes(payload.data)
		if err != nil {
			t.Fatalf("encode %s payload: %v", payload.kind, err)
		}
		relative := filepath.ToSlash(filepath.Join("payloads", payload.kind, payload.id+".json"))
		entries = append(entries, InventoryEntry{Kind: payload.kind, PortableID: payload.id, Path: relative, Blob: verifyTestBlob(body)})
		bodies[relative] = body
	}
	sort.Slice(entries, func(left int, right int) bool { return entries[left].Path < entries[right].Path })
	manifest := Manifest{
		Kind: Kind, ConvoRelayVersion: "test", TerminalStatus: "completed",
		SessionPayload:     "payloads/root_session/session.json",
		TranscriptPayload:  "payloads/participant_transcript/transcript.json",
		DiagnosticsPayload: "payloads/diagnostics/diagnostics.json",
		PayloadInventory:   entries,
	}
	manifest.InventoryDigest, _ = semanticjson.SemanticJSONDigest(entries)
	manifest.ManifestDigest, _ = semanticjson.SemanticJSONDigest(manifestDigestMaterial(manifest))
	if err := Validate(manifest); err != nil {
		t.Fatalf("validate test manifest: %v", err)
	}
	for relative, body := range bodies {
		filename := filepath.Join(directory, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", relative, err)
		}
		if err := os.WriteFile(filename, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}
	manifestBody, err := semanticjson.SemanticJSONBytes(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestBody, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return directory
}

func verifyTestBlob(body []byte) plan.BlobRef {
	digest := blobRefForBytes(body, "application/json")
	return digest
}
