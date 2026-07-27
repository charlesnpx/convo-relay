package contracts

import "strings"

type isolationEnum struct {
	field  string
	values map[string]bool
}

var isolationEnums = []isolationEnum{
	{field: "mechanism", values: map[string]bool{"inherited": true, "detached_writable_git_worktree": true, "unknown": true, "unsupported": true}},
	{field: "source_copy_separation", values: map[string]bool{"yes": true, "no": true, "unknown": true, "unsupported": true}},
	{field: "source_write_control", values: map[string]bool{"none": true, "post_run_detection": true, "unknown": true, "unsupported": true}},
	{field: "filesystem_containment", values: map[string]bool{"none": true, "unknown": true, "unsupported": true}},
	{field: "network_isolation", values: map[string]bool{"none": true, "unknown": true, "unsupported": true}},
	{field: "process_containment", values: map[string]bool{"none": true, "unknown": true, "unsupported": true}},
	{field: "same_user_security_boundary", values: map[string]bool{"none": true, "unknown": true, "unsupported": true}},
}

func WorkspaceIsolationReportRecord(fields map[string]any) (map[string]any, error) {
	record, _ := Materialize(fields).(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	record["schema_version"] = WorkspaceIsolationReportV1
	return ValidateWorkspaceIsolationReportRecord(record)
}

func ValidateWorkspaceIsolationReportRecord(value any) (map[string]any, error) {
	object, err := requireObjectValue(value, "workspace isolation report")
	if err != nil {
		return nil, err
	}
	if _, err := RequireStringVersion(object, "isolation_report"); err != nil {
		return nil, err
	}
	base, ok := object["base_identity"].(map[string]any)
	if !ok {
		return nil, NewValidationError("workspace isolation report base_identity must be an object")
	}
	for _, field := range []string{"object_format", "head_commit", "head_tree"} {
		if base[field] != nil {
			value, ok := base[field].(string)
			if !ok || strings.TrimSpace(value) == "" {
				return nil, NewValidationError("workspace isolation report base_identity %s must be null or a non-empty string", field)
			}
		}
	}
	for _, enum := range isolationEnums {
		value, ok := object[enum.field].(string)
		if !ok || !enum.values[value] {
			return nil, NewValidationError("workspace isolation report %s is invalid", enum.field)
		}
	}
	unknown, ok := object["unknown_dimensions"].([]any)
	if !ok {
		return nil, NewValidationError("workspace isolation report unknown_dimensions must be an array")
	}
	seen := map[string]bool{}
	for _, raw := range unknown {
		dimension, ok := raw.(string)
		if !ok || seen[dimension] {
			return nil, NewValidationError("workspace isolation report unknown_dimensions must contain unique strings")
		}
		seen[dimension] = true
	}
	return object, nil
}
