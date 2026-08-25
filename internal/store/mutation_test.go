package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactPersistenceRejectsEscapingLocationsBeforeCreatingSession(t *testing.T) {
	payload := map[string]any{"kind": "recipe", "schema_version": 1, "id": "unsafe"}
	tests := []struct {
		name string
		save func(*Store) error
	}{
		{
			name: "contract artifact ID",
			save: func(st *Store) error {
				_, err := st.SaveContractArtifact("recipes", "../../../outside", payload, "recipe:unsafe")
				return err
			},
		},
		{
			name: "wrapped artifact ID",
			save: func(st *Store) error {
				_, err := st.SaveArtifact("notes", "../../../outside", map[string]any{"message": "unsafe"})
				return err
			},
		},
		{
			name: "category",
			save: func(st *Store) error {
				_, err := st.SaveContractArtifact("../../outside", "artifact", payload, "recipe:unsafe")
				return err
			},
		},
		{
			name: "dot category targeting artifact index",
			save: func(st *Store) error {
				_, err := st.SaveArtifact(".", "index", map[string]any{"message": "unsafe"})
				return err
			},
		},
		{
			name: "normalized category targeting artifact index",
			save: func(st *Store) error {
				_, err := st.SaveArtifact("foo/..", "index", map[string]any{"message": "unsafe"})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			sessionDir := filepath.Join(parent, "session")
			if err := test.save(New(sessionDir)); err == nil {
				t.Fatal("escaping artifact location unexpectedly succeeded")
			}
			if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
				t.Fatalf("artifact validation created session directory, err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(parent, "outside.json")); !os.IsNotExist(err) {
				t.Fatalf("artifact validation wrote outside category, err = %v", err)
			}
		})
	}
}

func TestAtomicWriteFileCreatesParentsAndOverwrites(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "nested", "payload.json")

	if err := AtomicWriteFile(path, []byte(`{"value":"first"}`)); err != nil {
		t.Fatalf("atomic write first: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first write: %v", err)
	}
	if string(data) != `{"value":"first"}` {
		t.Fatalf("first write = %s", data)
	}

	if err := AtomicWriteFile(path, []byte(`{"value":"second"}`)); err != nil {
		t.Fatalf("atomic write overwrite: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overwrite: %v", err)
	}
	if string(data) != `{"value":"second"}` {
		t.Fatalf("overwrite = %s", data)
	}
}

func TestAtomicWriteFileRemovesTempFileOnRenameFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir target directory: %v", err)
	}

	if err := AtomicWriteFile(path, []byte("payload")); err == nil {
		t.Fatalf("atomic write unexpectedly succeeded over directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root entries: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".target.json.") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}
