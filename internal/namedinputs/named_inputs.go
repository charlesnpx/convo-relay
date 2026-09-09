// Package namedinputs ingests declared files into a session's blob store.
package namedinputs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/v2/internal/blobstore"
	"github.com/charlesnpx/convo-relay/v2/internal/eventlog"
	"github.com/charlesnpx/convo-relay/v2/internal/session"
)

const binaryMediaType = "application/octet-stream"

// Limits bounds the files read during one named-input ingestion. Zero means
// that dimension is unbounded.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// Binding is one CLI-level name=path declaration.
type Binding struct {
	Name string
	Path string
}

// Prepared contains source bytes read once and their immutable plan reference.
// The source path is intentionally not retained after this boundary.
type Prepared struct {
	Input session.Input
	body  []byte
}

// Read reads each source once, applies the configured limits, and computes the
// blob reference that will be embedded in the plan.
func Read(bindings []Binding, sourceAnchor string, limits Limits) ([]Prepared, error) {
	prepared := make([]Prepared, 0, len(bindings))
	seen := map[string]bool{}
	var total int64
	for _, binding := range bindings {
		name := strings.TrimSpace(binding.Name)
		if name == "" || strings.ContainsAny(name, "\r\n\x00") {
			return nil, errors.New("named input name is required and must not contain control characters")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate named input %q", name)
		}
		seen[name] = true
		path := strings.TrimSpace(binding.Path)
		if path == "" {
			return nil, fmt.Errorf("named input %q has no source path", name)
		}
		if !filepath.IsAbs(path) && strings.TrimSpace(sourceAnchor) != "" {
			path = filepath.Join(sourceAnchor, path)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, fmt.Errorf("inspect named input %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("named input %q is not a regular file", name)
		}
		if limits.MaxFileBytes > 0 && info.Size() > limits.MaxFileBytes {
			return nil, fmt.Errorf("named input %q exceeds %d bytes", name, limits.MaxFileBytes)
		}
		body, err := readLimited(absolute, limits.MaxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("read named input %q: %w", name, err)
		}
		total += int64(len(body))
		if limits.MaxTotalBytes > 0 && total > limits.MaxTotalBytes {
			return nil, fmt.Errorf("named inputs exceed %d bytes", limits.MaxTotalBytes)
		}
		sum := sha256.Sum256(body)
		prepared = append(prepared, Prepared{
			Input: session.Input{Name: name, Contents: []blobstore.BlobRef{{
				SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), MediaType: binaryMediaType,
			}}},
			body: append([]byte(nil), body...),
		})
	}
	return prepared, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.Reader(file)
	if limit > 0 {
		reader = io.LimitReader(file, limit+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(body)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return body, nil
}

// Inputs returns the plan-level blob references in declaration order.
func Inputs(prepared []Prepared) []session.Input {
	inputs := make([]session.Input, 0, len(prepared))
	for _, item := range prepared {
		inputs = append(inputs, item.Input)
	}
	return inputs
}

// Persist stores each prepared body and appends the matching input.ingested
// event only after blob durability has been established.
func Persist(sess *session.Session, prepared []Prepared) error {
	if sess == nil {
		return errors.New("session is required")
	}
	if len(prepared) == 0 {
		return nil
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return err
	}
	for _, item := range prepared {
		if len(item.Input.Contents) != 1 {
			return fmt.Errorf("named input %q must contain exactly one payload", item.Input.Name)
		}
		content := item.Input.Contents[0]
		stored, err := blobs.PutBytes(item.body, content.MediaType)
		if err != nil {
			return err
		}
		if !stored.Equal(content) {
			return fmt.Errorf("ingested named input %q has a digest mismatch", item.Input.Name)
		}
	}
	writer, err := sess.EventWriter(blobs)
	if err != nil {
		return err
	}
	defer writer.Close()
	for index, item := range prepared {
		content := item.Input.Contents[0]
		if _, err := writer.Append(eventlog.NewEvent(
			fmt.Sprintf("input-ingested-%d-%d", index, time.Now().UnixNano()),
			time.Now(),
			eventlog.InputIngestedPayload{LogicalName: item.Input.Name, Content: content},
		)); err != nil {
			return err
		}
	}
	return nil
}

// Materialize writes named inputs from verified blobs into a disposable input
// directory. It never reads a retained source file.
func Materialize(sess *session.Session, inputs []session.Input, destination string) error {
	if sess == nil {
		return errors.New("session is required")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	blobs, err := sess.BlobStore(blobstore.Limits{})
	if err != nil {
		return err
	}
	for _, input := range inputs {
		if filepath.Base(input.Name) != input.Name || input.Name == "." || input.Name == "" {
			return fmt.Errorf("unsafe named input path %q", input.Name)
		}
		if len(input.Contents) != 1 {
			return fmt.Errorf("named input %q must contain exactly one payload for materialization", input.Name)
		}
		content := input.Contents[0]
		reader, err := blobs.Open(content)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		path := filepath.Join(destination, input.Name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return err
		}
		if err := VerifyPath(path, content); err != nil {
			return err
		}
	}
	return nil
}

// VerifyPath establishes the sole retained-path property: its bytes still
// match the blob digest recorded in the plan.
func VerifyPath(path string, ref blobstore.BlobRef) error {
	if err := blobstore.ValidateRef(ref); err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return fmt.Errorf("named input digest mismatch at %s", path)
	}
	return nil
}
