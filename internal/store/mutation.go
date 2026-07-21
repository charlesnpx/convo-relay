package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/model"
)

type EventOptions struct {
	EventID    string
	Timestamp  string
	Visibility string
}

func (s *Store) EnsureSession() error {
	return os.MkdirAll(s.Root, 0o755)
}

func (s *Store) SaveMeta(meta model.SessionMeta) error {
	return s.writeIndentedJSONFile(filepath.Join(s.Root, "meta.json"), meta.ToMap())
}

func (s *Store) SaveMetaMap(meta map[string]any) error {
	return s.SaveMeta(model.NewSessionMeta(meta))
}

func (s *Store) SaveTranscript(transcript model.Transcript) error {
	return s.writeIndentedJSONFile(filepath.Join(s.Root, "transcript.json"), transcript.ToSlice())
}

func (s *Store) SaveTranscriptItems(transcript []any) error {
	typed, err := model.ParseTranscript(transcript)
	if err != nil {
		return err
	}
	return s.SaveTranscript(typed)
}

func (s *Store) SaveGraph(graph map[string]any) error {
	graph["version"] = 1
	return s.writeIndentedJSONFile(filepath.Join(s.Root, GraphFilename), graph)
}

func (s *Store) SaveArtifact(category string, artifactID string, data map[string]any) (map[string]any, error) {
	payload := map[string]any{
		"kind":           "artifact_payload",
		"schema_version": 1,
		"category":       category,
		"payload":        data,
	}
	return s.saveIndexedArtifact(category, artifactID, payload, "")
}

func (s *Store) LoadArtifact(ref map[string]any) (map[string]any, error) {
	payload, err := s.LoadArtifactPayloadRaw(ref)
	if err != nil {
		return nil, err
	}
	if payload["kind"] == "artifact_payload" && schemaVersionOne(payload["schema_version"]) {
		if inner, ok := payload["payload"].(map[string]any); ok {
			return inner, nil
		}
	}
	return payload, nil
}

func (s *Store) SaveContractArtifact(category string, artifactID string, payload map[string]any, refID string) (map[string]any, error) {
	if payload == nil {
		return nil, contracts.NewValidationError("contract artifact payload must be an object")
	}
	if !schemaVersionOne(payload["schema_version"]) || payload["kind"] == nil {
		return nil, contracts.NewValidationError("contract artifact payload must declare kind and schema_version 1")
	}
	return s.saveIndexedArtifact(category, artifactID, payload, refID)
}

func (s *Store) AppendSessionEventV1(eventType string, nodeID string, summary string, payload map[string]any, options EventOptions) (map[string]any, error) {
	if err := s.EnsureSession(); err != nil {
		return nil, err
	}
	seq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	visibility := strings.TrimSpace(options.Visibility)
	if visibility == "" {
		visibility = "operator"
	}
	eventID := strings.TrimSpace(options.EventID)
	if eventID == "" {
		eventID = newGraphID("evt")
	}
	timestamp := strings.TrimSpace(options.Timestamp)
	if timestamp == "" {
		timestamp = utcNow()
	}
	replacedPayload, err := s.replaceArtifactPathsWithRefs(payload)
	if err != nil {
		return nil, err
	}
	event, err := contracts.ValidateSessionEvent(map[string]any{
		"kind":           "session_event",
		"schema_version": 1,
		"seq":            seq,
		"event_id":       eventID,
		"timestamp":      timestamp,
		"event_type":     eventType,
		"node_id":        nodeID,
		"visibility":     visibility,
		"summary":        summary,
		"payload":        replacedPayload,
	})
	if err != nil {
		return nil, err
	}
	line, err := contracts.CanonicalJSONBytes(event)
	if err != nil {
		return nil, err
	}
	if err := appendLineSynced(filepath.Join(s.Root, EventsFilename), line); err != nil {
		return nil, err
	}
	return event, nil
}

func (s *Store) SaveProposal(proposal model.Proposal) error {
	payload := proposal.ToMap()
	proposalID := strings.TrimSpace(fmt.Sprint(payload["proposal_id"]))
	if proposalID == "" {
		return fmt.Errorf("proposal_id is required")
	}
	if err := s.EnsureSession(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := AtomicWriteFile(filepath.Join(s.Root, "proposals", proposalID+".json"), body); err != nil {
		return err
	}
	graph := s.loadGraph()
	proposals, ok := graph["proposals"].(map[string]any)
	if !ok {
		proposals = map[string]any{}
		graph["proposals"] = proposals
	}
	proposals[proposalID] = map[string]any{
		"proposal_id":          proposalID,
		"parent_node_id":       payload["parent_node_id"],
		"contested_lineage_id": payload["contested_lineage_id"],
		"selected_recipe_id":   payload["selected_recipe_id"],
		"status":               payload["status"],
		"delegated_question":   payload["delegated_question"],
		"updated_at":           utcNow(),
	}
	return s.SaveGraph(graph)
}

func (s *Store) SaveProposalMap(proposal map[string]any) error {
	return s.SaveProposal(model.NewProposal(proposal))
}

func (s *Store) LoadProposal(proposalID string) (model.Proposal, error) {
	cleanID := strings.TrimSpace(proposalID)
	if cleanID == "" {
		return model.Proposal{}, fmt.Errorf("proposal_id is required")
	}
	data, err := os.ReadFile(filepath.Join(s.Root, "proposals", cleanID+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return model.Proposal{}, NotFoundError{Message: fmt.Sprintf("No proposal '%s' for session %s", cleanID, s.SessionID())}
		}
		return model.Proposal{}, err
	}
	value, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		return model.Proposal{}, err
	}
	return model.NewProposal(value), nil
}

func (s *Store) LoadProposalMap(proposalID string) (map[string]any, error) {
	proposal, err := s.LoadProposal(proposalID)
	if err != nil {
		return nil, err
	}
	return proposal.ToMap(), nil
}

func (s *Store) ListProposals() ([]model.Proposal, error) {
	dir := filepath.Join(s.Root, "proposals")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []model.Proposal{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	proposals := make([]model.Proposal, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		value, err := contracts.DecodeJSONObjectBytes(data)
		if err != nil {
			continue
		}
		proposals = append(proposals, model.NewProposal(value))
	}
	return proposals, nil
}

func (s *Store) ListProposalMaps() ([]map[string]any, error) {
	proposals, err := s.ListProposals()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(proposals))
	for _, proposal := range proposals {
		items = append(items, proposal.ToMap())
	}
	return items, nil
}

func (s *Store) RecordAdmissionDecision(decision model.AdmissionDecision) error {
	payload := decision.ToMap()
	proposalID := strings.TrimSpace(fmt.Sprint(payload["proposal_id"]))
	if proposalID == "" {
		return fmt.Errorf("proposal_id is required")
	}
	graph := s.loadGraph()
	decisions, ok := graph["admission_decisions"].(map[string]any)
	if !ok {
		decisions = map[string]any{}
		graph["admission_decisions"] = decisions
	}
	decisions[proposalID] = payload
	return s.SaveGraph(graph)
}

func (s *Store) RecordAdmissionDecisionMap(decision map[string]any) error {
	return s.RecordAdmissionDecision(model.NewAdmissionDecision(decision))
}

func (s *Store) saveIndexedArtifact(category string, artifactID string, payload map[string]any, refID string) (map[string]any, error) {
	if err := s.EnsureSession(); err != nil {
		return nil, err
	}
	stableRelPath := filepath.ToSlash(filepath.Join("artifacts", category, artifactID+".json"))
	relPath, err := s.artifactWritePath(stableRelPath, payload)
	if err != nil {
		return nil, err
	}
	if refID == "" {
		refID = relPath
	}
	ref, err := contracts.ArtifactRefForPayload(refID, payload)
	if err != nil {
		return nil, err
	}
	body, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		return nil, err
	}
	payloadPath := filepath.Join(s.Root, filepath.FromSlash(relPath))
	rollbackFiles, err := captureArtifactWriteRollback([]string{
		payloadPath,
		filepath.Join(s.Root, "artifacts", ArtifactIndexFilename),
		filepath.Join(s.Root, GraphFilename),
	})
	if err != nil {
		return nil, err
	}
	if err := AtomicWriteFile(payloadPath, body); err != nil {
		return nil, rollbackArtifactWrite(rollbackFiles, err)
	}
	if err := s.updateArtifactIndex(ref, relPath); err != nil {
		return nil, rollbackArtifactWrite(rollbackFiles, err)
	}
	if err := s.recordArtifactGraphEntry(category, artifactID, relPath, ref); err != nil {
		return nil, rollbackArtifactWrite(rollbackFiles, err)
	}
	return ref, nil
}

type artifactWriteRollbackKind uint8

const (
	artifactWriteAbsent artifactWriteRollbackKind = iota
	artifactWriteRegular
	artifactWriteSymlink
	artifactWriteDirectory
)

type artifactWriteRollbackFile struct {
	path       string
	kind       artifactWriteRollbackKind
	mode       os.FileMode
	body       []byte
	linkTarget string
}

func captureArtifactWriteRollback(paths []string) ([]artifactWriteRollbackFile, error) {
	result := make([]artifactWriteRollbackFile, 0, len(paths))
	for _, path := range paths {
		snapshot := artifactWriteRollbackFile{path: path, kind: artifactWriteAbsent}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			result = append(result, snapshot)
			continue
		}
		if err != nil {
			return nil, err
		}
		snapshot.mode = info.Mode()
		switch {
		case info.Mode().IsRegular():
			snapshot.kind = artifactWriteRegular
			snapshot.body, err = os.ReadFile(path)
		case info.Mode()&os.ModeSymlink != 0:
			snapshot.kind = artifactWriteSymlink
			snapshot.linkTarget, err = os.Readlink(path)
		case info.IsDir():
			snapshot.kind = artifactWriteDirectory
		default:
			return nil, fmt.Errorf("artifact persistence target %s is not a regular file, symlink, directory, or absent", path)
		}
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func rollbackArtifactWrite(files []artifactWriteRollbackFile, cause error) error {
	errorsToJoin := []error{cause}
	for index := len(files) - 1; index >= 0; index-- {
		if err := files[index].restore(); err != nil {
			errorsToJoin = append(errorsToJoin, fmt.Errorf("restore %s: %w", files[index].path, err))
		}
	}
	return errors.Join(errorsToJoin...)
}

func (snapshot artifactWriteRollbackFile) restore() error {
	info, err := os.Lstat(snapshot.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	exists := err == nil
	switch snapshot.kind {
	case artifactWriteAbsent:
		if !exists {
			return nil
		}
		if info.IsDir() {
			return fmt.Errorf("rollback target changed into a directory")
		}
		return os.Remove(snapshot.path)
	case artifactWriteRegular:
		if exists && info.IsDir() {
			return fmt.Errorf("rollback target changed into a directory")
		}
		if err := AtomicWriteFile(snapshot.path, snapshot.body); err != nil {
			return err
		}
		return os.Chmod(snapshot.path, snapshot.mode.Perm())
	case artifactWriteSymlink:
		if exists {
			if info.IsDir() {
				return fmt.Errorf("rollback target changed into a directory")
			}
			if err := os.Remove(snapshot.path); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o755); err != nil {
			return err
		}
		return os.Symlink(snapshot.linkTarget, snapshot.path)
	case artifactWriteDirectory:
		if exists && info.IsDir() {
			return nil
		}
		return fmt.Errorf("rollback directory target changed during persistence")
	default:
		return fmt.Errorf("unknown artifact rollback state")
	}
}

func (s *Store) artifactWritePath(stableRelPath string, payload map[string]any) (string, error) {
	stablePath := filepath.Join(s.Root, filepath.FromSlash(stableRelPath))
	payloadDigest, err := contracts.ContractDigest(payload)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(stablePath); os.IsNotExist(err) {
		return stableRelPath, nil
	}
	if digest, ok := s.pathPayloadDigest(stableRelPath); ok && digest == payloadDigest {
		return stableRelPath, nil
	}
	ext := filepath.Ext(stableRelPath)
	stem := strings.TrimSuffix(stableRelPath, ext)
	digestHex := strings.TrimPrefix(payloadDigest, contracts.DigestPrefix)
	candidate := fmt.Sprintf("%s-%s%s", stem, digestHex[:12], ext)
	if _, err := os.Stat(filepath.Join(s.Root, filepath.FromSlash(candidate))); os.IsNotExist(err) {
		return candidate, nil
	}
	if digest, ok := s.pathPayloadDigest(candidate); ok && digest == payloadDigest {
		return candidate, nil
	}
	return fmt.Sprintf("%s-%s%s", stem, digestHex, ext), nil
}

func (s *Store) updateArtifactIndex(ref map[string]any, relPath string) error {
	index := s.ArtifactIndex()
	entries, _ := index["entries"].([]any)
	nextEntries := make([]any, 0, len(entries)+1)
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		candidate, ok := entry["ref"].(map[string]any)
		if ok && candidate["id"] == ref["id"] && candidate["digest"] == ref["digest"] {
			continue
		}
		nextEntries = append(nextEntries, entry)
	}
	nextEntries = append(nextEntries, map[string]any{"ref": ref, "path": filepath.ToSlash(relPath)})
	payload, err := contracts.ValidateArtifactIndex(map[string]any{
		"kind":           "artifact_index",
		"schema_version": 1,
		"entries":        nextEntries,
	})
	if err != nil {
		return err
	}
	body, err := contracts.CanonicalJSONBytes(payload)
	if err != nil {
		return err
	}
	return AtomicWriteFile(filepath.Join(s.Root, "artifacts", ArtifactIndexFilename), body)
}

func (s *Store) recordArtifactGraphEntry(category string, artifactID string, relPath string, ref map[string]any) error {
	graph := s.loadGraph()
	artifacts, ok := graph["artifacts"].(map[string]any)
	if !ok {
		artifacts = map[string]any{}
		graph["artifacts"] = artifacts
	}
	artifacts[category+"/"+artifactID] = map[string]any{
		"artifact_id": artifactID,
		"category":    category,
		"path":        filepath.ToSlash(relPath),
		"ref":         ref,
		"created_at":  utcNow(),
	}
	return s.SaveGraph(graph)
}

func (s *Store) replaceArtifactPathsWithRefs(value any) (map[string]any, error) {
	replaced, err := s.replaceArtifactPathsWithRefsValue(value, "")
	if err != nil {
		return nil, err
	}
	object, ok := replaced.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return object, nil
}

func (s *Store) replaceArtifactPathsWithRefsValue(value any, key string) (any, error) {
	if contracts.IsArtifactRef(value) {
		return contracts.ValidateArtifactRef(value)
	}
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for itemKey, itemValue := range typed {
			replaced, err := s.replaceArtifactPathsWithRefsValue(itemValue, itemKey)
			if err != nil {
				return nil, err
			}
			result[itemKey] = replaced
		}
		return result, nil
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			replaced, err := s.replaceArtifactPathsWithRefsValue(item, "")
			if err != nil {
				return nil, err
			}
			result = append(result, replaced)
		}
		return result, nil
	case string:
		if strings.HasSuffix(key, "_ref") {
			ref := s.artifactRefForPath(typed)
			if ref != nil {
				return ref, nil
			}
		}
		return typed, nil
	default:
		return value, nil
	}
}

func (s *Store) artifactRefForPath(relPath string) map[string]any {
	normalized := filepath.ToSlash(relPath)
	index := s.ArtifactIndex()
	entries, _ := index["entries"].([]any)
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok || entry["path"] != normalized {
			continue
		}
		ref, ok := entry["ref"].(map[string]any)
		if ok {
			return ref
		}
	}
	graph := s.loadGraph()
	artifacts, _ := graph["artifacts"].(map[string]any)
	for _, rawArtifact := range artifacts {
		artifact, ok := rawArtifact.(map[string]any)
		if !ok || artifact["path"] != normalized {
			continue
		}
		ref, ok := artifact["ref"].(map[string]any)
		if ok {
			return ref
		}
	}
	return nil
}

func (s *Store) nextEventSeq() (int, error) {
	lines, err := s.readRawEventLines()
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	return len(lines) + 1, nil
}

func (s *Store) writeIndentedJSONFile(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, body)
}

func AtomicWriteFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func appendLineSynced(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer handle.Close()
	if _, err := handle.Write(append(bytes.Clone(line), '\n')); err != nil {
		return err
	}
	return handle.Sync()
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func newGraphID(prefix string) string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func NewGraphID(prefix string) string {
	return newGraphID(prefix)
}
