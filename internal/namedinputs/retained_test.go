package namedinputs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestVerifyRetainedRejectsEveryExactLayoutMismatchWithoutContents(t *testing.T) {
	tests := []struct {
		name     string
		category string
		mutate   func(*testing.T, *store.Store, *Materialized) map[string]any
	}{
		{
			name:     "missing",
			category: IntegrityMismatchMissing,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				removeRetainedTarget(t, materialized, 0)
				return materialized.DescriptorRef
			},
		},
		{
			name:     "unexpected",
			category: IntegrityMismatchUnexpected,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				directory := materialized.Descriptor["directory"].(string)
				if err := os.WriteFile(filepath.Join(directory, "999999"), []byte("extra"), 0o444); err != nil {
					t.Fatalf("write unexpected input: %v", err)
				}
				return materialized.DescriptorRef
			},
		},
		{
			name:     "type",
			category: IntegrityMismatchType,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				target := retainedTarget(materialized, 0)
				if err := os.Remove(target); err != nil {
					t.Fatalf("remove retained target: %v", err)
				}
				if err := os.Symlink("elsewhere", target); err != nil {
					t.Fatalf("replace retained target with symlink: %v", err)
				}
				return materialized.DescriptorRef
			},
		},
		{
			name:     "mode",
			category: IntegrityMismatchMode,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				if err := os.Chmod(retainedTarget(materialized, 0), 0o644); err != nil {
					t.Fatalf("change retained mode: %v", err)
				}
				return materialized.DescriptorRef
			},
		},
		{
			name:     "size",
			category: IntegrityMismatchSize,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				target := retainedTarget(materialized, 0)
				if err := os.Chmod(target, 0o644); err != nil {
					t.Fatalf("make retained target writable: %v", err)
				}
				if err := os.WriteFile(target, []byte("different-size"), 0o444); err != nil {
					t.Fatalf("change retained size: %v", err)
				}
				if err := os.Chmod(target, 0o444); err != nil {
					t.Fatalf("restore retained mode: %v", err)
				}
				return materialized.DescriptorRef
			},
		},
		{
			name:     "digest",
			category: IntegrityMismatchDigest,
			mutate: func(t *testing.T, _ *store.Store, materialized *Materialized) map[string]any {
				target := retainedTarget(materialized, 0)
				if err := os.Chmod(target, 0o644); err != nil {
					t.Fatalf("make retained target writable: %v", err)
				}
				if err := os.WriteFile(target, []byte("omega"), 0o444); err != nil {
					t.Fatalf("change retained bytes: %v", err)
				}
				if err := os.Chmod(target, 0o444); err != nil {
					t.Fatalf("restore retained mode: %v", err)
				}
				return materialized.DescriptorRef
			},
		},
		{
			name:     "reordered descriptor",
			category: IntegrityMismatchReordered,
			mutate: func(t *testing.T, st *store.Store, materialized *Materialized) map[string]any {
				changed := cloneMap(materialized.Descriptor)
				inputs := changed["inputs"].([]any)
				inputs[0], inputs[1] = inputs[1], inputs[0]
				return saveRetainedDescriptor(t, st, changed)
			},
		},
		{
			name:     "path changed descriptor",
			category: IntegrityMismatchPath,
			mutate: func(t *testing.T, st *store.Store, materialized *Materialized) map[string]any {
				changed := cloneMap(materialized.Descriptor)
				entry := changed["inputs"].([]any)[0].(map[string]any)
				entry["materialized_path"] = filepath.Join(changed["directory"].(string), "different")
				return saveRetainedDescriptor(t, st, changed)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, materialized := retainedFixture(t)
			ref := test.mutate(t, st, materialized)
			err := VerifyRetained(context.Background(), st, ref, "participant", IntegrityBoundaryBeforeAttempt)
			var diagnosticErr *contracts.DiagnosticError
			if !errors.As(err, &diagnosticErr) || len(diagnosticErr.Diagnostics) != 1 {
				t.Fatalf("retained mismatch error = %T %v", err, err)
			}
			diagnostic := diagnosticErr.Diagnostics[0]
			if diagnostic.Code != DiagnosticCodeIntegrity ||
				diagnostic.Details["mismatch_category"] != test.category ||
				diagnostic.Details["role"] != "participant" ||
				diagnostic.Details["attempt_boundary"] != IntegrityBoundaryBeforeAttempt {
				t.Fatalf("retained mismatch diagnostic = %#v", diagnostic)
			}
			if test.name == "missing" &&
				(diagnostic.Details["input_name"] != "value" || diagnostic.Details["input_ordinal"] != 1) {
				t.Fatalf("early missing input diagnostic = %#v", diagnostic)
			}
			if containsKeyRecursive(diagnostic.ToMap(), "bytes") ||
				containsKeyRecursive(diagnostic.ToMap(), "content") {
				t.Fatalf("retained mismatch diagnostic exposed contents: %#v", diagnostic)
			}
		})
	}
}

func TestVerifyRetainedObservesCancellationWhileHashing(t *testing.T) {
	st, materialized := retainedFixture(t)
	ctx := newCancelAfterDoneChecksContext(3)
	if err := VerifyRetained(ctx, st, materialized.DescriptorRef, "initialization", IntegrityBoundaryInitialization); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retained verification = %v", err)
	}
}

func TestVerifyRetainedDoesNotRecreateMissingMaterializationDirectory(t *testing.T) {
	st, materialized := retainedFixture(t)
	directory := materialized.Descriptor["directory"].(string)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("remove retained materialization directory: %v", err)
	}
	err := VerifyRetained(context.Background(), st, materialized.DescriptorRef, "initialization", IntegrityBoundaryInitialization)
	var diagnosticErr *contracts.DiagnosticError
	if !errors.As(err, &diagnosticErr) {
		t.Fatalf("missing retained directory error = %T %v", err, err)
	}
	if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("verification recreated missing retained directory: %v", statErr)
	}
}

func TestVerifyRetainedReportsAnExtraEntryThatSortsBeforeExpectedFiles(t *testing.T) {
	st, materialized := retainedFixture(t)
	directory := materialized.Descriptor["directory"].(string)
	extra := filepath.Join(directory, "000000")
	if err := os.WriteFile(extra, []byte("extra"), 0o444); err != nil {
		t.Fatalf("write early-sorting extra: %v", err)
	}
	err := VerifyRetained(
		context.Background(),
		st,
		materialized.DescriptorRef,
		"inspection",
		"health",
	)
	var diagnosticErr *contracts.DiagnosticError
	if !errors.As(err, &diagnosticErr) || len(diagnosticErr.Diagnostics) != 1 {
		t.Fatalf("unexpected-entry error = %T %v", err, err)
	}
	diagnostic := diagnosticErr.Diagnostics[0]
	observed, _ := diagnostic.Details["observed"].(map[string]any)
	if diagnostic.Details["mismatch_category"] != IntegrityMismatchUnexpected ||
		filepath.Clean(stringValue(observed["path"])) != filepath.Clean(extra) {
		t.Fatalf("unexpected-entry diagnostic = %#v", diagnostic)
	}
}

func TestLoadRetainedReconstructsProjectionWithoutWriting(t *testing.T) {
	st, materialized := retainedFixture(t)
	before, err := os.ReadDir(materialized.Descriptor["directory"].(string))
	if err != nil {
		t.Fatalf("read retained directory before load: %v", err)
	}
	manifestRef := materialized.Descriptor["manifest_ref"].(map[string]any)
	loaded, err := LoadRetained(
		context.Background(),
		st,
		manifestRef,
		materialized.DescriptorRef,
		"recovery",
		IntegrityBoundaryRecovery,
	)
	if err != nil {
		t.Fatalf("load retained: %v", err)
	}
	after, err := os.ReadDir(materialized.Descriptor["directory"].(string))
	if err != nil {
		t.Fatalf("read retained directory after load: %v", err)
	}
	if len(before) != len(after) || len(loaded.ProviderInputs["inputs"].([]any)) != 2 ||
		!matchingArtifactRefs(loaded.DescriptorRef, materialized.DescriptorRef) {
		t.Fatalf("loaded retained projection = %#v", loaded)
	}
}

func retainedFixture(t *testing.T) (*store.Store, *Materialized) {
	t.Helper()
	root := t.TempDir()
	first := writeInputFile(t, root, "first.bin", []byte("alpha"))
	second := writeInputFile(t, root, "second.bin", []byte("bravo"))
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityMany, "application/octet-stream", 64, nil),
	})
	prepared, err := Prepare(Options{
		Contract: selected,
		Bindings: []string{"value=" + first, "value=" + second},
	})
	if err != nil {
		t.Fatalf("prepare retained fixture: %v", err)
	}
	st := store.New(filepath.Join(root, "session"))
	persisted, err := Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist retained fixture: %v", err)
	}
	materialized, err := MaterializeRetained(
		context.Background(),
		st,
		persisted.ManifestRef,
		filepath.Join(st.Root, "execution", "inputs"),
	)
	if err != nil {
		t.Fatalf("materialize retained fixture: %v", err)
	}
	if err := VerifyRetained(context.Background(), st, materialized.DescriptorRef, "initialization", IntegrityBoundaryInitialization); err != nil {
		t.Fatalf("verify retained fixture: %v", err)
	}
	return st, materialized
}

func retainedTarget(materialized *Materialized, index int) string {
	return materialized.Descriptor["inputs"].([]any)[index].(map[string]any)["materialized_path"].(string)
}

func removeRetainedTarget(t *testing.T, materialized *Materialized, index int) {
	t.Helper()
	if err := os.Remove(retainedTarget(materialized, index)); err != nil {
		t.Fatalf("remove retained target: %v", err)
	}
}

func saveRetainedDescriptor(t *testing.T, st *store.Store, descriptor map[string]any) map[string]any {
	t.Helper()
	identity, err := contracts.RootArtifactIdentityFor(contracts.RootArtifactKindRetainedInputs, 0)
	if err != nil {
		t.Fatalf("retained descriptor identity: %v", err)
	}
	ref, err := st.SaveContractArtifact(
		contracts.RootArtifactKindRetainedInputs,
		identity.ArtifactID,
		descriptor,
		identity.RefID,
	)
	if err != nil {
		t.Fatalf("save retained descriptor: %v", err)
	}
	return ref
}

type cancelAfterDoneChecksContext struct {
	context.Context

	mu          sync.Mutex
	done        chan struct{}
	checks      int
	cancelAfter int
	canceled    bool
}

func newCancelAfterDoneChecksContext(cancelAfter int) *cancelAfterDoneChecksContext {
	return &cancelAfterDoneChecksContext{
		Context:     context.Background(),
		done:        make(chan struct{}),
		cancelAfter: cancelAfter,
	}
}

func (c *cancelAfterDoneChecksContext) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	if !c.canceled && c.checks >= c.cancelAfter {
		close(c.done)
		c.canceled = true
	}
	return c.done
}

func (c *cancelAfterDoneChecksContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canceled {
		return context.Canceled
	}
	return nil
}
