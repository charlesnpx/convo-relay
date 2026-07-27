package portable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/inspect"
	"github.com/charlesnpx/convo-relay/internal/store"
)

type Options struct {
	ConvoRelayVersion string
}

type Result struct {
	Directory string
	Manifest  map[string]any
}

type sourceArtifact struct {
	ref     map[string]any
	payload map[string]any
}

type exportPayload struct {
	entry map[string]any
	body  []byte
}

func Export(sessionDir string, targetDir string, options Options) (*Result, error) {
	sessionRoot, err := canonicalDirectory(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("resolve portable export session: %w", err)
	}
	target, err := filepath.Abs(strings.TrimSpace(targetDir))
	if err != nil || strings.TrimSpace(targetDir) == "" {
		return nil, contracts.NewValidationError("portable export requires an output directory")
	}
	target = filepath.Clean(target)
	if _, err := os.Lstat(target); err == nil {
		return nil, contracts.NewValidationError("portable export target already exists")
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	st := store.New(sessionRoot)
	metaRecord, err := st.LoadMeta()
	if err != nil {
		return nil, err
	}
	meta := metaRecord.ToMap()
	if err := validatePortableSession(meta); err != nil {
		return nil, err
	}
	transcriptRecord, err := st.LoadTranscript()
	if err != nil {
		return nil, err
	}
	transcript := transcriptRecord.ToSlice()
	events, err := st.ReadEvents()
	if err != nil {
		return nil, err
	}
	artifacts, err := collectArtifactClosure(st, sessionRoot, meta, transcript, events)
	if err != nil {
		return nil, err
	}
	diagnostics := inspect.BuildRootInspectionReport(sessionRoot, meta, false)
	if diagnostics == nil {
		return nil, contracts.NewValidationError("portable export requires a direct root session")
	}
	payloads, err := buildExportPayloads(meta, transcript, diagnostics, artifacts)
	if err != nil {
		return nil, err
	}
	inventory := make([]any, 0, len(payloads))
	for _, payload := range payloads {
		inventory = append(inventory, payload.entry)
	}
	manifest, err := contracts.PortableExportManifest(map[string]any{
		"convo_relay_version": strings.TrimSpace(options.ConvoRelayVersion),
		"terminal_status":     strings.TrimSpace(stringValue(meta["status"])),
		"stop_reason":         emptyStringAsNil(stringValue(meta["stop_reason"])),
		"session_payload":     "payloads/root_session/session.json",
		"transcript_payload":  "payloads/participant_transcript/transcript.json",
		"diagnostics_payload": "payloads/diagnostics/diagnostics.json",
		"payload_inventory":   inventory,
	})
	if err != nil {
		return nil, err
	}
	if err := publishDirectory(target, payloads, manifest); err != nil {
		return nil, err
	}
	return &Result{Directory: target, Manifest: manifest}, nil
}

func validatePortableSession(meta map[string]any) error {
	if strings.TrimSpace(stringValue(meta["execution_kind"])) != "recipe" {
		return contracts.NewValidationError("portable export requires a direct root session")
	}
	if strings.TrimSpace(stringValue(meta["prompt_policy_version"])) != contracts.PromptPolicyV2 &&
		strings.TrimSpace(stringValue(meta["provider_retry"])) == "" {
		return contracts.NewValidationError("portable export requires a successor root session")
	}
	if meta["root_recovery_pending"] == true {
		return contracts.NewValidationError("portable export cannot represent a recovery-pending session as complete")
	}
	status := strings.TrimSpace(stringValue(meta["status"]))
	switch status {
	case "completed", "failed", "interrupted", "attention_required", "killed", "orphaned", "reducer_failed", "invalid_result":
		return nil
	default:
		return contracts.NewValidationError("portable export requires a terminal root session, got %q", status)
	}
}

func collectArtifactClosure(st *store.Store, sessionRoot string, meta map[string]any, transcript []any, events []map[string]any) ([]sourceArtifact, error) {
	index, err := strictSourceIndex(st, sessionRoot)
	if err != nil {
		return nil, err
	}
	queue := []map[string]any{}
	for _, value := range []any{meta, transcript, events} {
		queue = append(queue, contracts.FindArtifactRefs(value)...)
	}
	seen := map[string]bool{}
	result := []sourceArtifact{}
	for len(queue) > 0 {
		ref, err := contracts.ValidateArtifactRef(queue[0])
		queue = queue[1:]
		if err != nil {
			return nil, err
		}
		key := refKey(ref)
		if seen[key] {
			continue
		}
		entry, ok := index[key]
		if !ok {
			return nil, contracts.NewValidationError("portable export closure is missing artifact ref %s", ref["id"])
		}
		payload, err := loadIndexedPayload(sessionRoot, entry.path, ref)
		if err != nil {
			return nil, err
		}
		seen[key] = true
		result = append(result, sourceArtifact{ref: ref, payload: payload})
		queue = append(queue, contracts.FindArtifactRefs(payload)...)
	}
	for _, field := range []string{"recipe_ref", "root_recipe_plan_ref", "runtime_config_ref", "execution_workspace_ref", "isolation_report_ref"} {
		ref, ok := meta[field].(map[string]any)
		if !ok || !seen[refKey(ref)] {
			return nil, contracts.NewValidationError("portable export root session is missing required %s", field)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return refKey(result[i].ref) < refKey(result[j].ref)
	})
	return result, nil
}

type indexedSource struct {
	path string
}

func strictSourceIndex(st *store.Store, sessionRoot string) (map[string]indexedSource, error) {
	index, err := st.LoadArtifactIndexStrict()
	if err != nil {
		return nil, err
	}
	result := map[string]indexedSource{}
	paths := map[string]string{}
	for _, raw := range index["entries"].([]any) {
		entry := raw.(map[string]any)
		ref := entry["ref"].(map[string]any)
		relative := strings.TrimSpace(stringValue(entry["path"]))
		if relative == "" || strings.Contains(relative, "\\") || path.Clean(relative) != relative ||
			!strings.HasPrefix(relative, "artifacts/") {
			return nil, contracts.NewValidationError("artifact ref %s has an out-of-root index path", ref["id"])
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(sessionRoot, filepath.FromSlash(relative)))
		if err != nil {
			return nil, err
		}
		inside, err := filepath.Rel(sessionRoot, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
			return nil, contracts.NewValidationError("artifact ref %s resolves outside the session root", ref["id"])
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return nil, contracts.NewValidationError("artifact ref %s does not resolve to a regular payload", ref["id"])
		}
		key := refKey(ref)
		if prior, exists := paths[resolved]; exists && prior != key {
			return nil, contracts.NewValidationError("artifact index maps multiple refs to one payload path")
		}
		paths[resolved] = key
		result[key] = indexedSource{path: resolved}
	}
	return result, nil
}

func loadIndexedPayload(sessionRoot string, resolved string, ref map[string]any) (map[string]any, error) {
	inside, err := filepath.Rel(sessionRoot, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return nil, contracts.NewValidationError("artifact ref %s resolves outside the session root", ref["id"])
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	payload, err := contracts.DecodeStrictJSONObjectBytes(body)
	if err != nil {
		return nil, err
	}
	digest, err := contracts.PayloadDigest(payload)
	if err != nil {
		return nil, err
	}
	if digest != ref["digest"] {
		return nil, contracts.NewValidationError("artifact ref %s digest mismatch", ref["id"])
	}
	return payload, nil
}

func buildExportPayloads(meta map[string]any, transcript []any, diagnostics map[string]any, artifacts []sourceArtifact) ([]exportPayload, error) {
	portableIDs := make(map[string]string, len(artifacts))
	for index, artifact := range artifacts {
		portableIDs[refKey(artifact.ref)] = fmt.Sprintf("artifact-%06d", index+1)
	}
	payloads := []exportPayload{}
	for _, item := range []struct {
		kind string
		id   string
		data any
	}{
		{kind: "root_session", id: "session", data: portableSessionProjection(meta)},
		{kind: "participant_transcript", id: "transcript", data: transcript},
		{kind: "diagnostics", id: "diagnostics", data: diagnostics},
	} {
		projected, err := replaceArtifactRefs(item.data, portableIDs)
		if err != nil {
			return nil, err
		}
		payload, err := newExportPayload(item.kind, item.id, projected, "")
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	for _, artifact := range artifacts {
		kind := portableKind(stringValue(artifact.payload["kind"]))
		id := portableIDs[refKey(artifact.ref)]
		projected, err := portableArtifactProjection(artifact.payload, portableIDs)
		if err != nil {
			return nil, err
		}
		payload, err := newExportPayload(kind, id, projected, stringValue(artifact.ref["id"]))
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	sort.Slice(payloads, func(i, j int) bool {
		return stringValue(payloads[i].entry["path"]) < stringValue(payloads[j].entry["path"])
	})
	return payloads, nil
}

func newExportPayload(kind string, id string, value any, sourceArtifactID string) (exportPayload, error) {
	body, err := contracts.CanonicalJSONBytes(value)
	if err != nil {
		return exportPayload{}, err
	}
	entry := map[string]any{
		"kind":         kind,
		"portable_id":  id,
		"path":         path.Join("payloads", kind, id+".json"),
		"media_type":   "application/json",
		"size_bytes":   len(body),
		"digest_class": string(contracts.DigestClassRawBytes),
		"digest":       contracts.RawBytesDigest(body),
	}
	if sourceArtifactID != "" {
		entry["source_artifact_id"] = sourceArtifactID
	}
	return exportPayload{entry: entry, body: body}, nil
}

func portableArtifactProjection(payload map[string]any, portableIDs map[string]string) (map[string]any, error) {
	value, err := replaceArtifactRefs(payload, portableIDs)
	if err != nil {
		return nil, err
	}
	projected := value.(map[string]any)
	switch stringValue(projected["kind"]) {
	case contracts.RootArtifactKindExecutionWorkspace:
		delete(projected, "identity")
		deleteMapKeys(projected["source"], "git_root", "launch_cwd")
		deleteMapKeys(projected["source_after"], "git_root", "launch_cwd")
	case contracts.RootArtifactKindNamedInputManifest:
		deleteArrayMapKeys(projected["inputs"], "source_path")
	case contracts.RootArtifactKindRetainedInputs:
		delete(projected, "directory")
		deleteArrayMapKeys(projected["inputs"], "materialized_path")
	case contracts.RootArtifactKindReducerAttempt:
		deleteMapKeys(projected["provider_state"], "cwd", "settings_path")
	case "artifact_payload":
		inner, _ := projected["payload"].(map[string]any)
		switch stringValue(projected["category"]) {
		case "runtime_config":
			delete(inner, "settings_path")
		case "launch_context", "input_bundles":
			delete(inner, "source_path")
		case "facilitator_outputs":
			deleteMapKeys(inner["provider_state"], "cwd", "settings_path")
		}
		if stringValue(inner["kind"]) == "transient_recipe_file" || stringValue(inner["kind"]) == "input_bundle" {
			delete(inner, "source_path")
		}
	}
	return projected, nil
}

func replaceArtifactRefs(value any, portableIDs map[string]string) (any, error) {
	if contracts.IsArtifactRef(value) {
		ref := value.(map[string]any)
		portableID, ok := portableIDs[refKey(ref)]
		if !ok {
			return nil, contracts.NewValidationError("portable export projection is missing artifact ref %s", ref["id"])
		}
		return map[string]any{"kind": "portable_payload_ref", "portable_id": portableID}, nil
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			projected, err := replaceArtifactRefs(item, portableIDs)
			if err != nil {
				return nil, err
			}
			result[key] = projected
		}
		return result, nil
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			projected, err := replaceArtifactRefs(item, portableIDs)
			if err != nil {
				return nil, err
			}
			result = append(result, projected)
		}
		return result, nil
	default:
		return typed, nil
	}
}

func deleteMapKeys(value any, keys ...string) {
	object, _ := value.(map[string]any)
	for _, key := range keys {
		delete(object, key)
	}
}

func deleteArrayMapKeys(value any, keys ...string) {
	items, _ := value.([]any)
	for _, item := range items {
		deleteMapKeys(item, keys...)
	}
}

func portableSessionProjection(meta map[string]any) map[string]any {
	result := map[string]any{
		"kind":                     "portable_root_session",
		"terminal_status":          meta["status"],
		"stop_reason":              meta["stop_reason"],
		"execution_phase":          meta["execution_phase"],
		"recipe_id":                meta["recipe_id"],
		"task":                     meta["task"],
		"result_source":            meta["result_source"],
		"validation_status":        meta["validation_status"],
		"provider_retry":           meta["provider_retry"],
		"prompt_policy":            meta["prompt_policy"],
		"prompt_context":           meta["prompt_context"],
		"participant_turns":        meta["participant_turns"],
		"actual_participant_turns": meta["actual_participant_turns"],
		"source_changed":           meta["source_changed"],
		"source_mutated":           meta["source_mutated"],
		"source_before_digest":     meta["source_before_digest"],
		"source_after_digest":      meta["source_after_digest"],
	}
	refs := map[string]any{}
	for key, value := range meta {
		if strings.HasSuffix(key, "_ref") {
			if ref, ok := value.(map[string]any); ok && contracts.IsArtifactRef(ref) {
				refs[key] = ref
			}
			continue
		}
		if strings.HasSuffix(key, "_refs") {
			items := contracts.FindArtifactRefs(value)
			if len(items) > 0 {
				values := make([]any, 0, len(items))
				for _, ref := range items {
					values = append(values, ref)
				}
				refs[key] = values
			}
		}
	}
	result["artifact_refs"] = refs
	return result
}

func publishDirectory(target string, payloads []exportPayload, manifest map[string]any) (returnErr error) {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, filepath.Base(target)+"-tmp-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			returnErr = errors.Join(returnErr, os.RemoveAll(temporary))
		}
	}()
	for _, payload := range payloads {
		relative := stringValue(payload.entry["path"])
		if err := writeSyncedFile(filepath.Join(temporary, filepath.FromSlash(relative)), payload.body); err != nil {
			return err
		}
	}
	manifestBody, err := contracts.CanonicalJSONBytes(manifest)
	if err != nil {
		return err
	}
	if err := writeSyncedFile(filepath.Join(temporary, "manifest.json"), manifestBody); err != nil {
		return err
	}
	if _, err := VerifyDirectory(temporary); err != nil {
		return err
	}
	if err := syncDirectory(temporary); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return contracts.NewValidationError("portable export target appeared during publication")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	published = true
	return syncDirectory(parent)
}

func writeSyncedFile(filename string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if errors.Is(err, fs.ErrInvalid) {
		err = nil
	}
	return errors.Join(err, closeErr)
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
		return "", contracts.NewValidationError("path must resolve to a directory")
	}
	return filepath.Clean(resolved), nil
}

func portableKind(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "artifact"
	}
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), ".")
	if result == "" {
		return "artifact"
	}
	return result
}

func refKey(ref map[string]any) string {
	return stringValue(ref["id"]) + "\x00" + stringValue(ref["digest"])
}

func emptyStringAsNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
