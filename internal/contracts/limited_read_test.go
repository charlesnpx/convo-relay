package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countingReader struct {
	reader *bytes.Reader
	read   int
}

func (r *countingReader) Read(data []byte) (int, error) {
	count, err := r.reader.Read(data)
	r.read += count
	return count, err
}

func TestReadBytesLimitedEnforcesRawByteCeiling(t *testing.T) {
	exact, err := ReadBytesLimited(strings.NewReader("1234"), 4)
	if err != nil || string(exact) != "1234" {
		t.Fatalf("exact read = %q, %v", exact, err)
	}

	reader := &countingReader{reader: bytes.NewReader([]byte("123456789"))}
	if _, err := ReadBytesLimited(reader, 4); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("oversized read error = %v", err)
	}
	if reader.read != 5 {
		t.Fatalf("oversized reader consumed %d bytes, want max+1", reader.read)
	}

	if _, err := ReadBytesLimited(nil, 4); err == nil {
		t.Fatal("nil reader unexpectedly accepted")
	}
	if _, err := ReadBytesLimited(strings.NewReader(""), 0); err == nil {
		t.Fatal("nonpositive limit unexpectedly accepted")
	}
}

func TestReadFileBytesLimitedChecksStatAndReadBoundaries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(path, []byte("1234"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	if data, err := ReadFileBytesLimited(path, 4); err != nil || string(data) != "1234" {
		t.Fatalf("read exact file = %q, %v", data, err)
	}
	if _, err := ReadFileBytesLimited(path, 3); err == nil || !strings.Contains(err.Error(), "exceeds 3 bytes") {
		t.Fatalf("oversized file error = %v", err)
	}
	if _, err := ReadFileBytesLimited(root, 10); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory error = %v", err)
	}
}
