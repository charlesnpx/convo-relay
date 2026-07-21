//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package namedinputs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/charlesnpx/convo-relay/internal/integration"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestPrepareRejectsFIFOAndUnreadableFilesWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 64, nil),
	})
	fifo := filepath.Join(root, "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	_, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + fifo}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileNotRegular)

	if os.Geteuid() == 0 {
		t.Skip("root can read permission-zero files")
	}
	unreadable := writeInputFile(t, root, "unreadable.bin", []byte("secret"))
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatalf("chmod unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	_, err = Prepare(Options{Contract: selected, Bindings: []string{"value=" + unreadable}})
	requireDiagnosticCode(t, err, DiagnosticCodeFileUnavailable)
}

func TestMaterializeRejectsSymlinkedSessionDirectoryComponents(t *testing.T) {
	root := t.TempDir()
	source := writeInputFile(t, root, "value.bin", []byte("value"))
	selected := selectedContract(t, map[string]any{
		"value": inputDeclaration(true, integration.CardinalityOne, "application/octet-stream", 16, nil),
	})
	prepared, err := Prepare(Options{Contract: selected, Bindings: []string{"value=" + source}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st := store.New(filepath.Join(root, "session"))
	persisted, err := Persist(st, prepared)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	executionLink := filepath.Join(st.Root, "execution")
	if err := os.Symlink(outside, executionLink); err != nil {
		t.Fatalf("symlink execution directory: %v", err)
	}
	_, err = Materialize(st, persisted.ManifestRef, filepath.Join(executionLink, "inputs"))
	requireDiagnosticCode(t, err, DiagnosticCodeIntegrity)
	if _, err := os.Stat(filepath.Join(outside, "inputs")); !os.IsNotExist(err) {
		t.Fatalf("materialization escaped through a symlink: %v", err)
	}
}

func TestSecureOpenRejectsSymlinksAndFIFOsWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	regular := writeInputFile(t, root, "regular.bin", []byte("value"))
	symlink := filepath.Join(root, "symlink.bin")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if handle, _, err := openNamedInputFile(symlink); !errors.Is(err, errNotRegular) {
		if handle != nil {
			_ = handle.Close()
		}
		t.Fatalf("secure symlink open error = %v", err)
	}

	fifo := filepath.Join(root, "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		handle, _, err := openNamedInputFile(fifo)
		if handle != nil {
			_ = handle.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("secure FIFO open error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("secure FIFO open blocked")
	}
}
