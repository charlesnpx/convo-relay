package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/charlesnpx/convo-relay/v2/internal/semanticjson"
	"github.com/charlesnpx/convo-relay/v2/plan"
	"github.com/charlesnpx/convo-relay/v2/result"
)

// VerifyOptions supplies caller-owned identity expectations. Empty values leave
// the corresponding expectation unchecked; bundle-internal consistency is
// always checked.
type VerifyOptions struct {
	// ExpectedPlanDigest is the semantic-JSON digest of the plan the caller
	// submitted. An empty value does not impose a caller digest expectation.
	ExpectedPlanDigest string
	// ExpectedSessionID is the session identity the caller expects. An empty value
	// does not impose a caller session identity expectation.
	ExpectedSessionID string
}

// Verification is the typed result of verifying one portable bundle directory.
// Manifest, Session, Transcript, and Diagnostics are decoded from the bundle;
// PayloadCount counts every inventoried payload file.
type Verification struct {
	// Manifest is the verified relay.bundle/v1 manifest.
	Manifest Manifest
	// Session is the verified root-session payload.
	Session SessionPayload
	// Transcript is the verified participant-transcript payload.
	Transcript []result.TranscriptEntry
	// Diagnostics is the verified diagnostics payload.
	Diagnostics result.Diagnostics
	// PayloadCount is the number of inventoried payload files in the bundle.
	PayloadCount int
}

// SessionPayload is the typed root-session payload in a portable bundle. Plan
// and Root reuse the public plan and result document types; no parallel payload
// document definitions are introduced here.
type SessionPayload struct {
	// Plan is the immutable execution plan that produced the bundle.
	Plan plan.Plan `json:"plan"`
	// TerminalStatus is the exported terminal status and must agree with the
	// manifest and Root.Status.
	TerminalStatus string `json:"terminal_status"`
	// StopReason is the optional terminal stop reason and must agree with the
	// manifest; an empty value represents the bundle's JSON null.
	StopReason string `json:"stop_reason"`
	// ResultSource records the plan's result source and must agree with Root.Result
	// and Plan.Result.Source.
	ResultSource string `json:"result_source"`
	// ValidationStatus records result validation and must agree with Root.Result.
	ValidationStatus string `json:"validation_status"`
	// WorkspaceContentSource identifies the workspace content represented by the
	// portable result; it may be empty for older compatible payloads.
	WorkspaceContentSource string `json:"workspace_content_source"`
	// WorkingTreeChangesIncluded records whether working-tree changes were used.
	WorkingTreeChangesIncluded bool `json:"working_tree_changes_included"`
	// Root is the typed root execution projection.
	Root result.Root `json:"root"`
}

// VerifyPortableDirectory verifies a closed portable bundle directory. With no
// options it retains the verifier's existing meaning; supplied expectations add
// checks without changing the bundle format.
func VerifyPortableDirectory(directory string, options ...VerifyOptions) (Verification, error) {
	if len(options) > 1 {
		return Verification{}, errors.New("portable bundle accepts at most one verification options value")
	}
	var expected VerifyOptions
	if len(options) == 1 {
		expected = options[0]
	}

	root, err := canonicalDirectory(directory)
	if err != nil {
		return Verification{}, err
	}
	manifestBody, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return Verification{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		return Verification{}, err
	}
	if err := Validate(manifest); err != nil {
		return Verification{}, err
	}

	expectedFiles := map[string]bool{"manifest.json": true}
	required := map[string]bool{
		"root_session:session":              false,
		"participant_transcript:transcript": false,
		"diagnostics:diagnostics":           false,
	}
	inputEntries := map[string]plan.BlobRef{}
	var sessionPayload *SessionPayload
	var transcript []result.TranscriptEntry
	var diagnostics result.Diagnostics
	payloadCount := 0

	for _, entry := range manifest.PayloadInventory {
		key := entry.Kind + ":" + entry.PortableID
		isInput := entry.Kind == "input"
		if _, accepted := required[key]; !accepted && !isInput {
			return Verification{}, fmt.Errorf("portable export contains unsupported payload %s", key)
		}
		if isInput && strings.TrimSpace(entry.PortableID) == "" {
			return Verification{}, errors.New("portable export input payload has an empty identity")
		}
		if !isInput {
			required[key] = true
		}

		expectedFiles[entry.Path] = true
		filename := filepath.Join(root, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() {
			return Verification{}, fmt.Errorf("portable export payload %s is missing or not a regular file", entry.Path)
		}
		body, err := os.ReadFile(filename)
		if err != nil {
			return Verification{}, err
		}
		if err := verifyPayloadBytes(entry, body, info.Size()); err != nil {
			return Verification{}, err
		}

		if isInput {
			if entry.Blob.SHA256 != entry.PortableID {
				return Verification{}, fmt.Errorf("portable export input payload %s identity does not match its blob: portable_id=%q, blob.sha256=%q", entry.Path, entry.PortableID, entry.Blob.SHA256)
			}
			inputEntries[entry.PortableID] = entry.Blob
			payloadCount++
			continue
		}

		switch key {
		case "root_session:session":
			var rootFields map[string]json.RawMessage
			if err := json.Unmarshal(body, &rootFields); err == nil {
				if _, found := rootFields["kind"]; found {
					return Verification{}, errors.New("portable root session payload must omit kind; relay.bundle/v1 identifies it")
				}
			}
			var decoded SessionPayload
			if err := decodePayload(body, &decoded); err != nil {
				return Verification{}, fmt.Errorf("decode portable export payload %s: %w", entry.Path, err)
			}
			if err := validateSessionPayload(decoded); err != nil {
				return Verification{}, err
			}
			sessionPayload = &decoded
		case "participant_transcript:transcript":
			var decoded []result.TranscriptEntry
			if err := decodePayload(body, &decoded); err != nil {
				return Verification{}, fmt.Errorf("decode portable export payload %s: %w", entry.Path, err)
			}
			if err := validateTranscript(decoded); err != nil {
				return Verification{}, err
			}
			transcript = decoded
		case "diagnostics:diagnostics":
			var decoded result.Diagnostics
			if err := decodePayload(body, &decoded); err != nil {
				return Verification{}, fmt.Errorf("decode portable export payload %s: %w", entry.Path, err)
			}
			if err := validateDiagnostics(decoded); err != nil {
				return Verification{}, err
			}
			diagnostics = decoded
		}
		payloadCount++
	}

	for key, found := range required {
		if !found {
			return Verification{}, fmt.Errorf("portable export is missing required payload %s", key)
		}
	}
	if sessionPayload == nil {
		return Verification{}, errors.New("portable root session payload is required")
	}
	if sessionPayload.TerminalStatus != manifest.TerminalStatus {
		return Verification{}, mismatch("terminal_status", manifest.TerminalStatus, sessionPayload.TerminalStatus)
	}
	if sessionPayload.StopReason != optionalStringValue(manifest.StopReason) {
		return Verification{}, mismatch("stop_reason", optionalStringValue(manifest.StopReason), sessionPayload.StopReason)
	}

	if manifest.SessionPayload != path.Join("payloads", "root_session", "session.json") {
		return Verification{}, fmt.Errorf("portable bundle session_payload mismatch: expected=%q, actual=%q", path.Join("payloads", "root_session", "session.json"), manifest.SessionPayload)
	}
	if manifest.TranscriptPayload != path.Join("payloads", "participant_transcript", "transcript.json") {
		return Verification{}, fmt.Errorf("portable bundle transcript_payload mismatch: expected=%q, actual=%q", path.Join("payloads", "participant_transcript", "transcript.json"), manifest.TranscriptPayload)
	}
	if manifest.DiagnosticsPayload != path.Join("payloads", "diagnostics", "diagnostics.json") {
		return Verification{}, fmt.Errorf("portable bundle diagnostics_payload mismatch: expected=%q, actual=%q", path.Join("payloads", "diagnostics", "diagnostics.json"), manifest.DiagnosticsPayload)
	}

	expectedInputs, err := portableInputRefs(sessionPayload.Plan)
	if err != nil {
		return Verification{}, err
	}
	for digest, ref := range expectedInputs {
		actual, found := inputEntries[digest]
		if !found {
			return Verification{}, fmt.Errorf("portable export is missing input payload %s", digest)
		}
		if !actual.Equal(ref) {
			return Verification{}, fmt.Errorf("portable export input payload %s does not match the root plan: expected=%#v, actual=%#v", digest, ref, actual)
		}
	}
	for digest, actual := range inputEntries {
		if _, expected := expectedInputs[digest]; !expected {
			return Verification{}, fmt.Errorf("portable export contains input payload %s not referenced by the root plan: actual=%#v", digest, actual)
		}
	}

	actualDigest, err := plan.Digest(sessionPayload.Plan)
	if err != nil {
		return Verification{}, fmt.Errorf("portable bundle plan_digest: %w", err)
	}
	if strings.TrimSpace(expected.ExpectedPlanDigest) != "" && actualDigest != expected.ExpectedPlanDigest {
		return Verification{}, mismatch("plan_digest", expected.ExpectedPlanDigest, actualDigest)
	}
	if strings.TrimSpace(expected.ExpectedSessionID) != "" && sessionPayload.Plan.SessionID != expected.ExpectedSessionID {
		return Verification{}, mismatch("session_id", expected.ExpectedSessionID, sessionPayload.Plan.SessionID)
	}

	if err := verifyClosedFileSet(root, expectedFiles); err != nil {
		return Verification{}, err
	}
	return Verification{
		Manifest:     manifest,
		Session:      *sessionPayload,
		Transcript:   transcript,
		Diagnostics:  diagnostics,
		PayloadCount: payloadCount,
	}, nil
}

func decodePayload(body []byte, destination any) error {
	return semanticjson.DecodeJSON(body, destination)
}

func validateSessionPayload(value SessionPayload) error {
	if err := plan.Validate(value.Plan); err != nil {
		return fmt.Errorf("validate portable root plan: %w", err)
	}
	if strings.TrimSpace(value.TerminalStatus) == "" {
		return errors.New("portable root session terminal_status is required")
	}
	if strings.TrimSpace(value.ResultSource) == "" {
		return errors.New("portable root session result_source is required")
	}
	if strings.TrimSpace(value.ValidationStatus) == "" {
		return errors.New("portable root session validation_status is required")
	}
	if err := validateRoot(value.Root); err != nil {
		return err
	}
	if value.Root.Status != value.TerminalStatus {
		return mismatch("terminal_status", value.TerminalStatus, value.Root.Status)
	}
	if value.Root.Result.Source != value.ResultSource {
		return mismatch("result_source", value.ResultSource, value.Root.Result.Source)
	}
	if value.Root.Result.Source != value.Plan.Result.Source {
		return mismatch("result.source", value.Plan.Result.Source, value.Root.Result.Source)
	}
	if value.Root.Result.ValidationStatus != value.ValidationStatus {
		return mismatch("validation_status", value.ValidationStatus, value.Root.Result.ValidationStatus)
	}
	return nil
}

func validateRoot(value result.Root) error {
	if strings.TrimSpace(value.ExecutionKind) == "" {
		return errors.New("result root.execution_kind is required")
	}
	if strings.TrimSpace(value.Status) == "" {
		return errors.New("result root.status is required")
	}
	if value.Turns.Configured < 0 || value.Turns.Completed < 0 {
		return errors.New("result root.turns counts must not be negative")
	}
	if value.Invocations != nil && value.Invocations.Count < 0 {
		return errors.New("result root invocation counts must not be negative")
	}
	if value.ReducerAttempts.Count < 0 {
		return errors.New("result root invocation counts must not be negative")
	}
	return nil
}

func validateTranscript(entries []result.TranscriptEntry) error {
	for index, entry := range entries {
		if entry.Round < 1 {
			return fmt.Errorf("result transcript[%d].round must be positive", index)
		}
		if strings.TrimSpace(entry.From) == "" {
			return fmt.Errorf("result transcript[%d].from is required", index)
		}
		if strings.TrimSpace(entry.Mode) == "" {
			return fmt.Errorf("result transcript[%d].mode is required", index)
		}
		if provider := entry.FacilitatorProviderResult; provider != nil {
			if strings.TrimSpace(provider.Backend) == "" {
				return fmt.Errorf("result transcript[%d].facilitator_provider_result.backend is required", index)
			}
			if strings.TrimSpace(provider.ActorID) == "" {
				return fmt.Errorf("result transcript[%d].facilitator_provider_result.actor_id is required", index)
			}
		}
	}
	return nil
}

func validateDiagnostics(value result.Diagnostics) error {
	for index, attempt := range value.AbandonedAttempts {
		if attempt.Attempt < 1 {
			return fmt.Errorf("result diagnostics.abandoned_attempts[%d].attempt must be positive", index)
		}
	}
	return nil
}

func portableInputRefs(value plan.Plan) (map[string]plan.BlobRef, error) {
	refs := map[string]plan.BlobRef{}
	for _, ref := range plan.BlobRefs(value) {
		if previous, exists := refs[ref.SHA256]; exists && !previous.Equal(ref) {
			return nil, fmt.Errorf("portable root plan references blob %s with conflicting metadata: expected=%#v, actual=%#v", ref.SHA256, previous, ref)
		}
		refs[ref.SHA256] = ref
	}
	return refs, nil
}

func verifyPayloadBytes(entry InventoryEntry, body []byte, fileSize int64) error {
	actual := blobRefForBytes(body, entry.Blob.MediaType)
	if fileSize != entry.Blob.Size || !actual.Equal(entry.Blob) {
		return fmt.Errorf("portable export payload %s size or digest mismatch: expected sha256=%q size=%d, actual sha256=%q size=%d", entry.Path, entry.Blob.SHA256, entry.Blob.Size, actual.SHA256, actual.Size)
	}
	return nil
}

func blobRefForBytes(body []byte, mediaType string) plan.BlobRef {
	digest := sha256.Sum256(body)
	return plan.BlobRef{
		SHA256:    hex.EncodeToString(digest[:]),
		Size:      int64(len(body)),
		MediaType: mediaType,
	}
}

func mismatch(field string, expected any, actual any) error {
	return fmt.Errorf("portable bundle %s mismatch: expected=%q, actual=%q", field, expected, actual)
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func verifyClosedFileSet(root string, expected map[string]bool) error {
	expectedDirectories := map[string]bool{}
	for filename := range expected {
		for directory := path.Dir(filename); directory != "."; directory = path.Dir(directory) {
			expectedDirectories[directory] = true
		}
	}
	return filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("portable export contains a symlink at %s", relative)
		}
		if entry.IsDir() {
			if expectedDirectories[relative] {
				return nil
			}
			return fmt.Errorf("portable export contains an unexpected directory %s", relative)
		}
		if !expected[relative] {
			return fmt.Errorf("portable export contains an unexpected file %s", relative)
		}
		return nil
	})
}

func canonicalDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("path must resolve to a directory")
	}
	return filepath.Clean(resolved), nil
}
