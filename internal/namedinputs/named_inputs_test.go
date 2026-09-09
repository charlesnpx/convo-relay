package namedinputs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/convo-relay/v2/internal/plan"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

func TestAlteredMaterializedNamedInputIsRejectedWithDigestMismatch(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Read([]Binding{{Name: "input", Path: source}}, "", Limits{MaxFileBytes: 64, MaxTotalBytes: 64})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	compiled, err := plan.FromFlags(plan.Flags{SessionID: "input-test", Task: "test", Agents: "codex,codex", Rounds: 1, Inputs: Inputs(prepared)})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	sess, err := session.Create(t.TempDir(), compiled)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := Persist(sess, prepared); err != nil {
		t.Fatalf("persist: %v", err)
	}
	destination := t.TempDir()
	if err := Materialize(sess, compiled.Inputs, destination); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	target := filepath.Join(destination, "input")
	if err := os.WriteFile(target, []byte("altered"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = VerifyPath(target, compiled.Inputs[0].Contents[0])
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VerifyPath error = %v, want digest mismatch", err)
	}
}
