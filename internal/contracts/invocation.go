package contracts

import (
	"encoding/base64"
	"strings"
)

func RenderedPromptRecord(data []byte) map[string]any {
	return map[string]any{
		"schema_version": RenderedPromptV1,
		"media_type":     "text/plain; charset=utf-8",
		"encoding":       "base64",
		"bytes_base64":   base64.StdEncoding.EncodeToString(data),
		"size_bytes":     len(data),
		"raw_digest":     RawBytesDigest(data),
	}
}

func ValidateRenderedPromptRecord(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "rendered prompt")
	if err != nil {
		return nil, err
	}
	if _, err := RequireStringVersion(object, "rendered_prompt"); err != nil {
		return nil, err
	}
	if object["media_type"] != "text/plain; charset=utf-8" || object["encoding"] != "base64" {
		return nil, NewValidationError("rendered prompt requires UTF-8 text encoded as base64")
	}
	encoded, ok := object["bytes_base64"].(string)
	if !ok {
		return nil, NewValidationError("rendered prompt bytes_base64 must be a string")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, NewValidationError("rendered prompt bytes_base64 is invalid")
	}
	size, ok := exactJSONInteger(object["size_bytes"])
	if !ok || size != len(data) || object["raw_digest"] != RawBytesDigest(data) {
		return nil, NewValidationError("rendered prompt size or raw digest mismatch")
	}
	object["size_bytes"] = size
	return object, nil
}

func ProviderInvocationRecord(fields map[string]any) (map[string]any, error) {
	record, _ := Materialize(fields).(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	record["schema_version"] = ProviderInvocationV1
	return ValidateProviderInvocationRecord(record)
}

func ValidateProviderInvocationRecord(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "provider invocation")
	if err != nil {
		return nil, err
	}
	if _, err := RequireStringVersion(object, "provider_invocation"); err != nil {
		return nil, err
	}
	invocationID, _ := object["invocation_id"].(string)
	phase, _ := object["phase"].(string)
	actor, _ := object["actor"].(string)
	if strings.TrimSpace(invocationID) == "" || strings.TrimSpace(phase) == "" || strings.TrimSpace(actor) == "" {
		return nil, NewValidationError("provider invocation requires invocation_id, phase, and actor")
	}
	attempt, ok := exactJSONInteger(object["runner_attempt"])
	if !ok || attempt < 1 {
		return nil, NewValidationError("provider invocation runner_attempt must be a positive integer")
	}
	if object["provider_launch_attempted"] != true && object["provider_launch_attempted"] != false {
		return nil, NewValidationError("provider invocation provider_launch_attempted must be boolean")
	}
	if object["provider_retry"] != "allow" && object["provider_retry"] != "forbid" {
		return nil, NewValidationError("provider invocation provider_retry must be allow or forbid")
	}
	if workingDirectory, _ := object["mapped_working_directory"].(string); !portableRelativePath(workingDirectory) {
		return nil, NewValidationError("provider invocation mapped_working_directory must be portable and relative")
	}
	object["runner_attempt"] = attempt
	if object["participant_ordinal"] != nil {
		ordinal, valid := exactJSONInteger(object["participant_ordinal"])
		if !valid || ordinal < 1 {
			return nil, NewValidationError("provider invocation participant_ordinal must be null or a positive integer")
		}
		object["participant_ordinal"] = ordinal
	}
	return object, nil
}

func portableRelativePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, ":") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == ".." {
			return false
		}
	}
	return true
}
