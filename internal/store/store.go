package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
)

const (
	EventsFilename        = "events.jsonl"
	GraphFilename         = "graph.json"
	ArtifactIndexFilename = "index.json"
)

type NotFoundError struct {
	Message string
}

func (e NotFoundError) Error() string {
	return e.Message
}

type Store struct {
	Root             string
	mutationObserver FileMutationObserver
}

func New(root string) *Store {
	return &Store{Root: root}
}

// FileMutationObserver lets a transaction persist an exact write intent
// before Store changes a session file. The observer is intentionally generic:
// Store does not interpret transaction state, and ordinary stores have no
// observer.
type FileMutationObserver interface {
	BeforeFileMutation(path string, body []byte, mode os.FileMode) (FileMutationPlan, error)
	AfterFileMutation(plan FileMutationPlan, committed bool) error
}

// FileMutationPlan identifies the exact transaction-created temporary file
// reserved by an observer. Store writes through that existing file rather than
// creating an unjournaled inode of its own. RequireAbsentTarget makes the
// commit a no-replace publication; observers must first preserve and remove
// any owned prior target as part of their durable intent protocol.
type FileMutationPlan struct {
	ID                  string
	TemporaryPath       string
	RequireAbsentTarget bool
}

func NewWithFileMutationObserver(root string, observer FileMutationObserver) *Store {
	return &Store{Root: root, mutationObserver: observer}
}

func (s *Store) SetFileMutationObserver(observer FileMutationObserver) {
	if s == nil {
		return
	}
	s.mutationObserver = observer
}

func (s *Store) SessionID() string {
	cleaned := filepath.Clean(s.Root)
	return filepath.Base(cleaned)
}

func (s *Store) ReadEvents() ([]map[string]any, error) {
	rawLines, err := s.readRawEventLines()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []map[string]any{}, nil
		}
		return nil, err
	}

	events := make([]map[string]any, 0, len(rawLines))
	for _, rawLine := range rawLines {
		value, err := contracts.DecodeJSONBytes(rawLine)
		if err != nil {
			return nil, contracts.NewValidationError("invalid event JSON: %v", err)
		}
		event, err := contracts.ValidateSessionEvent(value)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (s *Store) ArtifactIndex() map[string]any {
	index, err := s.loadArtifactIndexStrict()
	if err != nil {
		return defaultArtifactIndex()
	}
	return index
}

func (s *Store) LoadGraph() map[string]any {
	return s.loadGraph()
}

func (s *Store) LoadMeta() (model.SessionMeta, error) {
	payload, err := s.decodeJSONObjectFile(filepath.Join(s.Root, "meta.json"))
	if err != nil {
		return model.EmptySessionMeta(), err
	}
	return model.NewSessionMeta(payload), nil
}

func (s *Store) LoadTranscript() (model.Transcript, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, "transcript.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.EmptyTranscript(), nil
		}
		return model.EmptyTranscript(), err
	}
	value, err := contracts.DecodeJSONBytes(data)
	if err != nil {
		return model.EmptyTranscript(), err
	}
	return model.ParseTranscript(value)
}

func (s *Store) ResolveArtifactRef(refID string, digest string) (map[string]any, error) {
	index := s.ArtifactIndex()
	entries, _ := index["entries"].([]any)

	var matches []map[string]any
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		candidate, ok := entry["ref"].(map[string]any)
		if !ok || candidate["id"] != refID {
			continue
		}
		if digest != "" && candidate["digest"] != digest {
			continue
		}
		ref, err := contracts.ValidateArtifactRef(candidate)
		if err != nil {
			return nil, err
		}
		matches = append(matches, ref)
	}

	if len(matches) == 0 {
		suffix := ""
		if digest != "" {
			suffix = " with digest " + digest
		}
		return nil, NotFoundError{Message: fmt.Sprintf("No artifact ref '%s'%s", refID, suffix)}
	}

	distinct := map[string]bool{}
	for _, match := range matches {
		distinct[match["id"].(string)+"\x00"+match["digest"].(string)] = true
	}
	if digest == "" && len(distinct) > 1 {
		return nil, contracts.NewValidationError("artifact ref '%s' is ambiguous; provide --digest", refID)
	}
	return matches[0], nil
}

func (s *Store) ArtifactPathForRef(ref map[string]any) (string, error) {
	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return "", err
	}
	return s.artifactPathForRef(artifactRef)
}

func (s *Store) LoadArtifactPayloadRaw(ref map[string]any) (map[string]any, error) {
	if !contracts.IsArtifactRef(ref) {
		return nil, contracts.NewValidationError("load_artifact_payload_raw requires an artifact_ref object")
	}
	return s.loadArtifactPayloadForRef(ref)
}

func (s *Store) ValidateStrictV1(mode string) (map[string]any, error) {
	if mode == "" {
		mode = "native_v1"
	}
	if mode != "native_v1" && mode != "legacy_export_synthesized" && mode != "permissive" {
		return nil, fmt.Errorf("mode must be native_v1, legacy_export_synthesized, or permissive")
	}

	graph := s.loadGraph()
	hasGraphState := hasGraphState(graph)
	rawLines, err := s.readRawEventLines()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		rawLines = nil
	}
	if mode == "native_v1" && hasGraphState && len(rawLines) == 0 {
		return nil, contracts.NewValidationError("non-empty graph snapshot has no v1 event authority")
	}

	events := make([]map[string]any, 0, len(rawLines))
	for index, rawLine := range rawLines {
		value, err := contracts.DecodeJSONBytes(rawLine)
		if err != nil {
			return nil, contracts.NewValidationError("event line %d is not valid JSON", index+1)
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, contracts.NewValidationError("event line %d must be an object", index+1)
		}
		if object["kind"] != "session_event" {
			return nil, contracts.NewValidationError("mixed legacy/v1 event lines are not valid native v1")
		}
		event, err := contracts.ValidateSessionEvent(object)
		if err != nil {
			return nil, err
		}
		canonical, err := contracts.CanonicalJSONBytes(event)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(rawLine, canonical) {
			return nil, contracts.NewValidationError("event line %d is not canonical JSON", index+1)
		}
		if event["seq"] != index+1 {
			return nil, contracts.NewValidationError("event seq values must be contiguous starting at 1")
		}
		events = append(events, event)
	}

	seen := map[string]bool{}
	for _, event := range events {
		for _, ref := range contracts.FindArtifactRefs(event["payload"]) {
			if err := s.VerifyArtifactRef(ref, seen); err != nil {
				return nil, err
			}
		}
	}

	seqMin := 0
	if len(events) > 0 {
		seqMin = 1
	}
	return map[string]any{
		"mode":        mode,
		"event_count": len(events),
		"seq_min":     seqMin,
		"seq_max":     len(events),
	}, nil
}

func (s *Store) VerifyArtifactRef(ref map[string]any, seen map[string]bool) error {
	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return err
	}
	key := artifactRef["id"].(string) + "\x00" + artifactRef["digest"].(string)
	if seen[key] {
		return nil
	}
	seen[key] = true

	payload, err := s.loadArtifactPayloadForRef(artifactRef)
	if err != nil {
		return err
	}
	if !schemaVersionOne(payload["schema_version"]) {
		return contracts.NewValidationError("artifact ref %s payload lacks schema_version 1", artifactRef["id"])
	}
	kind, ok := payload["kind"].(string)
	if !ok || kind == "" {
		return contracts.NewValidationError("artifact ref %s payload lacks kind", artifactRef["id"])
	}
	for _, nested := range contracts.FindArtifactRefs(payload) {
		if err := s.VerifyArtifactRef(nested, seen); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadArtifactPayloadForRef(ref map[string]any) (map[string]any, error) {
	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return nil, err
	}
	relPath, err := s.artifactPathForRef(artifactRef)
	if err != nil {
		return nil, contracts.NewValidationError("artifact ref %s is missing", artifactRef["id"])
	}
	payload, err := s.decodeJSONObjectFile(filepath.Join(s.Root, filepath.FromSlash(relPath)))
	if err != nil {
		return nil, contracts.NewValidationError("artifact ref %s does not point to an object", artifactRef["id"])
	}
	digest, err := contracts.ContractDigest(payload)
	if err != nil {
		return nil, err
	}
	if digest != artifactRef["digest"] {
		return nil, contracts.NewValidationError("artifact ref %s digest mismatch", artifactRef["id"])
	}
	return payload, nil
}

func (s *Store) artifactPathForRef(ref map[string]any) (string, error) {
	artifactRef, err := contracts.ValidateArtifactRef(ref)
	if err != nil {
		return "", err
	}
	index := s.ArtifactIndex()
	entries, _ := index["entries"].([]any)
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		candidate, ok := entry["ref"].(map[string]any)
		if !ok {
			continue
		}
		if candidate["id"] == artifactRef["id"] && candidate["digest"] == artifactRef["digest"] {
			relPath, _ := entry["path"].(string)
			if digest, ok := s.pathPayloadDigest(relPath); ok && digest == artifactRef["digest"] {
				return relPath, nil
			}
			return "", contracts.NewValidationError("artifact ref %s digest mismatch", artifactRef["id"])
		}
	}

	refID := artifactRef["id"].(string)
	if strings.HasPrefix(refID, "artifacts/") {
		if _, err := os.Stat(filepath.Join(s.Root, filepath.FromSlash(refID))); err == nil {
			if digest, ok := s.pathPayloadDigest(refID); ok && digest == artifactRef["digest"] {
				return refID, nil
			}
			return "", contracts.NewValidationError("artifact ref %s digest mismatch", artifactRef["id"])
		}
	}
	return "", NotFoundError{Message: fmt.Sprintf("No artifact for ref %s", artifactRef["id"])}
}

func (s *Store) loadArtifactIndexStrict() (map[string]any, error) {
	path := filepath.Join(s.Root, "artifacts", ArtifactIndexFilename)
	payload, err := s.decodeJSONFile(path)
	if err != nil {
		return nil, err
	}
	return contracts.ValidateArtifactIndex(payload)
}

func (s *Store) decodeJSONObjectFile(path string) (map[string]any, error) {
	value, err := s.decodeJSONFile(path)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, contracts.NewValidationError("JSON payload must be an object")
	}
	return object, nil
}

func (s *Store) decodeJSONFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return contracts.DecodeJSONBytes(data)
}

func (s *Store) pathPayloadDigest(relPath string) (string, bool) {
	payload, err := s.decodeJSONObjectFile(filepath.Join(s.Root, filepath.FromSlash(relPath)))
	if err != nil {
		return "", false
	}
	digest, err := contracts.ContractDigest(payload)
	if err != nil {
		return "", false
	}
	return digest, true
}

func (s *Store) readRawEventLines() ([][]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, EventsFilename))
	if err != nil {
		return nil, err
	}
	split := bytes.Split(data, []byte{'\n'})
	lines := make([][]byte, 0, len(split))
	for _, line := range split {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		copied := append([]byte(nil), line...)
		lines = append(lines, copied)
	}
	return lines, nil
}

func (s *Store) loadGraph() map[string]any {
	graph := defaultGraph()
	payload, err := s.decodeJSONObjectFile(filepath.Join(s.Root, GraphFilename))
	if err != nil {
		return graph
	}
	for key, value := range payload {
		graph[key] = value
	}
	return graph
}

func defaultGraph() map[string]any {
	return map[string]any{
		"version":             1,
		"root_node_id":        "root",
		"nodes":               map[string]any{},
		"edges":               []any{},
		"proposals":           map[string]any{},
		"admission_decisions": map[string]any{},
		"artifacts":           map[string]any{},
		"backend_profiles":    map[string]any{},
		"relay_recipes":       map[string]any{},
	}
}

func defaultArtifactIndex() map[string]any {
	return map[string]any{
		"kind":           "artifact_index",
		"schema_version": 1,
		"entries":        []any{},
	}
}

func hasGraphState(graph map[string]any) bool {
	return collectionLen(graph["nodes"]) > 0 ||
		collectionLen(graph["edges"]) > 0 ||
		collectionLen(graph["proposals"]) > 0 ||
		collectionLen(graph["admission_decisions"]) > 0
}

func collectionLen(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		return len(typed)
	case []any:
		return len(typed)
	default:
		return 0
	}
}

func schemaVersionOne(value any) bool {
	ref := map[string]any{
		"kind":           "artifact_ref",
		"schema_version": value,
		"id":             "schema-version-test",
		"digest":         "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	_, err := contracts.ValidateArtifactRef(ref)
	return err == nil
}
