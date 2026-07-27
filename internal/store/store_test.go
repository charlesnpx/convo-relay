package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
)

func TestResolveArtifactRefRequiresDigestForDuplicateIDs(t *testing.T) {
	sessionDir := t.TempDir()
	first := map[string]any{
		"kind":           "recipe",
		"schema_version": 1,
		"id":             "dup",
		"value":          "first",
	}
	second := map[string]any{
		"kind":           "recipe",
		"schema_version": 1,
		"id":             "dup",
		"value":          "second",
	}
	firstRef := writeArtifact(t, sessionDir, "artifacts/recipes/dup.json", "recipe:dup", first)
	secondRef := writeArtifact(t, sessionDir, "artifacts/recipes/dup-second.json", "recipe:dup", second)
	writeIndex(t, sessionDir, []map[string]any{
		{"ref": firstRef, "path": "artifacts/recipes/dup.json"},
		{"ref": secondRef, "path": "artifacts/recipes/dup-second.json"},
	})

	st := New(sessionDir)
	if _, err := st.ResolveArtifactRef("recipe:dup", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ResolveArtifactRef without digest error = %v, want ambiguous", err)
	}
	resolved, err := st.ResolveArtifactRef("recipe:dup", secondRef["digest"].(string))
	if err != nil {
		t.Fatalf("ResolveArtifactRef with digest: %v", err)
	}
	if resolved["digest"] != secondRef["digest"] {
		t.Fatalf("resolved digest = %v, want %v", resolved["digest"], secondRef["digest"])
	}
	payload, err := st.LoadArtifactPayloadRaw(resolved)
	if err != nil {
		t.Fatalf("load resolved artifact: %v", err)
	}
	if payload["value"] != "second" {
		t.Fatalf("resolved payload value = %v, want second", payload["value"])
	}
}

func TestSaveContractArtifactAcceptsExplicitRegisteredV2Envelope(t *testing.T) {
	st := New(t.TempDir())
	payload := map[string]any{
		"kind":                  contracts.RootArtifactKindRootRecipePlan,
		"schema_version":        2,
		"prompt_policy_version": contracts.PromptPolicyV2,
	}
	ref, err := st.SaveContractArtifact("root_recipe_plan", "selected", payload, "root_recipe_plan:selected")
	if err != nil {
		t.Fatalf("save v2 contract artifact: %v", err)
	}
	loaded, err := st.LoadArtifactPayloadRaw(ref)
	if err != nil {
		t.Fatalf("load v2 contract artifact: %v", err)
	}
	if version, err := contracts.RequireNumericVersion(loaded, contracts.ContractRootArtifact); err != nil || version != 2 {
		t.Fatalf("loaded schema_version = %#v, version=%d err=%v", loaded["schema_version"], version, err)
	}
}

func writeArtifact(t *testing.T, sessionDir string, relPath string, refID string, payload map[string]any) map[string]any {
	t.Helper()
	digest, err := contracts.ContractDigest(payload)
	if err != nil {
		t.Fatalf("digest %s: %v", relPath, err)
	}
	body, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		t.Fatalf("canonical %s: %v", relPath, err)
	}
	path := filepath.Join(sessionDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return map[string]any{
		"kind":           "artifact_ref",
		"schema_version": 1,
		"id":             refID,
		"digest":         digest,
	}
}

func writeIndex(t *testing.T, sessionDir string, entries []map[string]any) {
	t.Helper()
	rawEntries := make([]any, 0, len(entries))
	for _, entry := range entries {
		rawEntries = append(rawEntries, entry)
	}
	index := map[string]any{
		"kind":           "artifact_index",
		"schema_version": 1,
		"entries":        rawEntries,
	}
	body, err := contracts.CanonicalJSONBytes(index)
	if err != nil {
		t.Fatalf("canonical index: %v", err)
	}
	path := filepath.Join(sessionDir, "artifacts", "index.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}
