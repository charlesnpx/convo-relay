package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	SchemaVersion = 1
	DigestPrefix  = "sha256:"
)

var (
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)

	digestExcludedKeys = map[string]bool{
		"absolute_path":       true,
		"artifact_id":         true,
		"created_at":          true,
		"generated_at":        true,
		"local_path":          true,
		"path":                true,
		"pid":                 true,
		"process_id":          true,
		"relay_pid":           true,
		"session_dir":         true,
		"snapshot_updated_at": true,
		"storage_id":          true,
		"updated_at":          true,
	}
)

type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

func NewValidationError(format string, args ...any) ValidationError {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

func DecodeJSONBytes(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return Materialize(value), nil
}

func DecodeJSONObjectBytes(data []byte) (map[string]any, error) {
	value, err := DecodeJSONBytes(data)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, NewValidationError("JSON payload must be an object")
	}
	return object, nil
}

func CanonicalJSONBytes(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Materialize(value)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func CanonicalJSONText(value any) (string, error) {
	data, err := CanonicalJSONBytes(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func ContractDigest(value any) (string, error) {
	canonical, err := CanonicalJSONBytes(digestMaterial(value))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func ArtifactRefForPayload(artifactID string, payload any) (map[string]any, error) {
	digest, err := ContractDigest(payload)
	if err != nil {
		return nil, err
	}
	return ValidateArtifactRef(map[string]any{
		"kind":           "artifact_ref",
		"schema_version": SchemaVersion,
		"id":             artifactID,
		"digest":         digest,
	})
}

func IsArtifactRef(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	id, idOK := object["id"].(string)
	digest, digestOK := object["digest"].(string)
	return object["kind"] == "artifact_ref" &&
		schemaVersionIsOne(object["schema_version"]) &&
		idOK && id != "" &&
		digestOK && digest != ""
}

func ValidateArtifactRef(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "artifact_ref payload")
	if err != nil {
		return nil, err
	}
	if err := validateAllowedKeys("artifact_ref", object, []string{"kind", "schema_version", "id", "digest"}); err != nil {
		return nil, err
	}
	if object["kind"] != "artifact_ref" {
		return nil, NewValidationError("artifact_ref payload has wrong kind")
	}
	if !schemaVersionIsOne(object["schema_version"]) {
		return nil, NewValidationError("artifact_ref payload requires numeric schema_version 1")
	}
	id, err := requireString(object, "id", false)
	if err != nil {
		return nil, err
	}
	digest, err := requireString(object, "digest", false)
	if err != nil {
		return nil, err
	}
	if !refIDRE.MatchString(id) {
		return nil, NewValidationError("artifact_ref.id contains unsupported characters")
	}
	if !digestRE.MatchString(digest) {
		return nil, NewValidationError("artifact_ref.digest must be sha256:<64 lowercase hex>")
	}
	return map[string]any{
		"kind":           "artifact_ref",
		"schema_version": SchemaVersion,
		"id":             id,
		"digest":         digest,
	}, nil
}

func FindArtifactRefs(value any) []map[string]any {
	if IsArtifactRef(value) {
		object, _ := value.(map[string]any)
		return []map[string]any{cloneObject(object)}
	}

	var refs []map[string]any
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			refs = append(refs, FindArtifactRefs(typed[key])...)
		}
	case []any:
		for _, item := range typed {
			refs = append(refs, FindArtifactRefs(item)...)
		}
	}
	return refs
}

func ValidateSessionEvent(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "session_event payload")
	if err != nil {
		return nil, err
	}
	if err := validateAllowedKeys("session_event", object, []string{
		"kind",
		"schema_version",
		"seq",
		"event_id",
		"timestamp",
		"event_type",
		"node_id",
		"visibility",
		"summary",
		"payload",
	}); err != nil {
		return nil, err
	}
	if object["kind"] != "session_event" {
		return nil, NewValidationError("session_event payload has wrong kind")
	}
	if !schemaVersionIsOne(object["schema_version"]) {
		return nil, NewValidationError("session_event payload requires numeric schema_version 1")
	}
	seq, err := requirePositiveInt(object, "seq")
	if err != nil {
		return nil, err
	}
	eventID, err := requireString(object, "event_id", false)
	if err != nil {
		return nil, err
	}
	timestamp, err := requireString(object, "timestamp", false)
	if err != nil {
		return nil, err
	}
	eventType, err := requireString(object, "event_type", false)
	if err != nil {
		return nil, err
	}
	nodeID, err := requireString(object, "node_id", true)
	if err != nil {
		return nil, err
	}
	visibility, err := requireChoice(object, "visibility", []string{"operator", "parent", "internal"})
	if err != nil {
		return nil, err
	}
	summary, err := requireString(object, "summary", true)
	if err != nil {
		return nil, err
	}
	payload, err := requireObject(object, "payload")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"kind":           "session_event",
		"schema_version": SchemaVersion,
		"seq":            seq,
		"event_id":       strings.TrimSpace(eventID),
		"timestamp":      strings.TrimSpace(timestamp),
		"event_type":     strings.TrimSpace(eventType),
		"node_id":        nodeID,
		"visibility":     visibility,
		"summary":        summary,
		"payload":        payload,
	}, nil
}

func ValidateArtifactIndex(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "artifact_index payload")
	if err != nil {
		return nil, err
	}
	if err := validateAllowedKeys("artifact_index", object, []string{"kind", "schema_version", "entries"}); err != nil {
		return nil, err
	}
	if object["kind"] != "artifact_index" {
		return nil, NewValidationError("artifact_index payload has wrong kind")
	}
	if !schemaVersionIsOne(object["schema_version"]) {
		return nil, NewValidationError("artifact_index payload requires numeric schema_version 1")
	}
	rawEntries, ok := object["entries"].([]any)
	if !ok {
		return nil, NewValidationError("entries must be a list")
	}

	seen := map[string]bool{}
	entries := make([]any, 0, len(rawEntries))
	for index, raw := range rawEntries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, NewValidationError("entries[%d] must be an object", index)
		}
		refValue, ok := entry["ref"]
		if !ok {
			return nil, NewValidationError("ref must be an object")
		}
		ref, err := ValidateArtifactRef(refValue)
		if err != nil {
			return nil, err
		}
		path, err := requireString(entry, "path", false)
		if err != nil {
			return nil, err
		}
		key := ref["id"].(string) + "\x00" + ref["digest"].(string)
		if seen[key] {
			return nil, NewValidationError("artifact_index contains duplicate refs")
		}
		seen[key] = true
		entries = append(entries, map[string]any{
			"ref":  ref,
			"path": strings.TrimSpace(path),
		})
	}

	return map[string]any{
		"kind":           "artifact_index",
		"schema_version": SchemaVersion,
		"entries":        entries,
	}, nil
}

func Materialize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = Materialize(item)
		}
		return result
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[fmt.Sprint(key)] = Materialize(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = Materialize(item)
		}
		return result
	default:
		return value
	}
}

func digestMaterial(value any) any {
	materialized := Materialize(value)
	switch typed := materialized.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if digestExcludedKeys[key] {
				continue
			}
			result[key] = digestMaterial(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = digestMaterial(item)
		}
		return result
	default:
		return materialized
	}
}

func requireObject(data map[string]any, key string) (map[string]any, error) {
	value, ok := data[key]
	if !ok {
		return nil, NewValidationError("%s must be an object", key)
	}
	return requireObjectValue(value, key)
}

func requireObjectValue(value any, label string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, NewValidationError("%s must be an object", label)
	}
	return cloneObject(object), nil
}

func requireString(data map[string]any, key string, allowEmpty bool) (string, error) {
	value, ok := data[key]
	if !ok {
		return "", NewValidationError("%s must be a string", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", NewValidationError("%s must be a string", key)
	}
	if !allowEmpty && strings.TrimSpace(text) == "" {
		return "", NewValidationError("%s cannot be empty", key)
	}
	return text, nil
}

func requireChoice(data map[string]any, key string, choices []string) (string, error) {
	value, err := requireString(data, key, false)
	if err != nil {
		return "", err
	}
	for _, choice := range choices {
		if value == choice {
			return value, nil
		}
	}
	sorted := append([]string(nil), choices...)
	sort.Strings(sorted)
	return "", NewValidationError("%s must be one of: %s", key, strings.Join(sorted, ", "))
}

func requirePositiveInt(data map[string]any, key string) (int, error) {
	value, ok := data[key]
	if !ok {
		return 0, NewValidationError("%s must be a positive integer", key)
	}
	integer, ok := integerValue(value)
	if !ok || integer < 1 {
		return 0, NewValidationError("%s must be a positive integer", key)
	}
	return integer, nil
}

func validateAllowedKeys(kind string, object map[string]any, allowed []string) error {
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[key] = true
	}
	var unknown []string
	for key := range object {
		if !allowedSet[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return NewValidationError("%s payload has unknown field(s): %s", kind, strings.Join(unknown, ", "))
	}
	return nil
}

func schemaVersionIsOne(value any) bool {
	if boolean, ok := value.(bool); ok && boolean {
		return false
	}
	switch typed := value.(type) {
	case int:
		return typed == SchemaVersion
	case int64:
		return typed == SchemaVersion
	case float64:
		return typed == SchemaVersion
	case json.Number:
		if integer, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
			return integer == SchemaVersion
		}
		floatValue, err := strconv.ParseFloat(typed.String(), 64)
		return err == nil && floatValue == SchemaVersion
	default:
		return false
	}
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case int:
		return typed, true
	case int64:
		if typed > math.MaxInt || typed < math.MinInt {
			return 0, false
		}
		return int(typed), true
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt || typed < math.MinInt {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		integer, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil || integer > math.MaxInt || integer < math.MinInt {
			return 0, false
		}
		return int(integer), true
	default:
		return 0, false
	}
}

func cloneObject(object map[string]any) map[string]any {
	result := make(map[string]any, len(object))
	for key, value := range object {
		result[key] = Materialize(value)
	}
	return result
}
