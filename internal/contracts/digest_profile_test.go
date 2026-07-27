package contracts

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRelayRootDigestsV1Fixtures(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "contracts", "relay-root-digests-v1.json")
	data, err := ReadFileBytesLimited(fixturePath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := DecodeStrictJSONObjectBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range fixture["cases"].([]any) {
		item := raw.(map[string]any)
		var digest string
		switch item["class"] {
		case string(DigestClassRawBytes):
			digest = RawBytesDigest([]byte(item["utf8"].(string)))
		case string(DigestClassSemanticJSON):
			canonical, canonicalErr := SemanticJSONBytes(item["value"])
			if canonicalErr != nil {
				t.Fatalf("%s canonical: %v", item["id"], canonicalErr)
			}
			if string(canonical) != item["canonical"] {
				t.Fatalf("%s canonical = %q, want %q", item["id"], canonical, item["canonical"])
			}
			digest, err = SemanticJSONDigest(item["value"])
		case string(DigestClassStorageEnvelope):
			digest, err = StorageEnvelopeDigest(item["kind"].(string), item["value"])
		default:
			t.Fatalf("unknown fixture class %v", item["class"])
		}
		if err != nil || digest != item["digest"] {
			t.Fatalf("%s digest = %q, want %q, err=%v", item["id"], digest, item["digest"], err)
		}
	}

	numbers := fixture["equivalent_numbers"].(map[string]any)
	for _, raw := range numbers["json"].([]any) {
		digest, digestErr := SemanticJSONDigestBytes([]byte(raw.(string)))
		if digestErr != nil || digest != numbers["digest"] {
			t.Fatalf("number %s digest = %q, want %q, err=%v", raw, digest, numbers["digest"], digestErr)
		}
	}
}

func TestRelayRootDigestsV1BindsNamesAndUsesExactStorageExclusions(t *testing.T) {
	base := map[string]any{"path": "one", "created_at": "two", "storage_id": "three"}
	first, err := SemanticJSONDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base["path"] = "changed"
	second, err := SemanticJSONDigest(base)
	if err != nil || first == second {
		t.Fatalf("semantic path member was excluded: %q %q %v", first, second, err)
	}

	workspace := map[string]any{"kind": RootArtifactKindExecutionWorkspace, "schema_version": 2, "digest_profile": DigestProfileV1, "identity": map[string]any{"session_dir": "/one"}, "base": map[string]any{"head_commit": "abc"}}
	first, err = StorageEnvelopeDigest(RootArtifactKindExecutionWorkspace, workspace)
	workspace["identity"] = map[string]any{"session_dir": "/two"}
	second, err = StorageEnvelopeDigest(RootArtifactKindExecutionWorkspace, workspace)
	if err != nil || first != second {
		t.Fatalf("runtime identity changed storage digest: %q %q %v", first, second, err)
	}
	workspace["base"].(map[string]any)["head_commit"] = "def"
	third, err := StorageEnvelopeDigest(RootArtifactKindExecutionWorkspace, workspace)
	if err != nil || second == third {
		t.Fatalf("semantic workspace base was excluded: %q %q %v", second, third, err)
	}
}

func TestEveryRootArtifactKindSupportsThePublishedProfile(t *testing.T) {
	for _, kind := range RootArtifactKinds() {
		payload, err := NormalizeRootArtifactVersion(kind, 2, map[string]any{"value": kind})
		if err != nil {
			t.Fatalf("%s normalize: %v", kind, err)
		}
		if payload["digest_profile"] != DigestProfileV1 {
			t.Fatalf("%s profile = %v", kind, payload["digest_profile"])
		}
		if _, err := PayloadDigest(payload); err != nil {
			t.Fatalf("%s digest: %v", kind, err)
		}
	}
	if _, err := PayloadDigest(map[string]any{"digest_profile": "unknown"}); err == nil {
		t.Fatal("unknown digest profile accepted")
	}
	if _, err := SemanticJSONDigestBytes([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate JSON member accepted")
	}
}

func TestNodeReproducesDigestFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	fixturePath, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "contracts", "relay-root-digests-v1.json"))
	scriptPath, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "verify_digest_fixtures.mjs"))
	output, err := exec.Command(node, scriptPath, fixturePath).CombinedOutput()
	if err != nil {
		t.Fatalf("node fixture verifier: %v\n%s", err, output)
	}
}

func TestSemanticJSONMarshalShapeIsJSON(t *testing.T) {
	encoded, err := SemanticJSONBytes(map[string]any{"number": json.Number("1.00e2")})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"number":1e2}` {
		t.Fatalf("canonical = %s", encoded)
	}
}
