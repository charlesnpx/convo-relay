package model

type ShowReport struct {
	SessionID  string         `json:"session_id,omitempty"`
	SessionDir string         `json:"session_dir,omitempty"`
	Fields     map[string]any `json:"-"`
}

type HealthReport struct {
	Scope     string         `json:"scope,omitempty"`
	Status    string         `json:"status,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Fields    map[string]any `json:"-"`
}

type ExportReport struct {
	SessionID string         `json:"session_id,omitempty"`
	Fields    map[string]any `json:"-"`
}

func NewShowReport(fields map[string]any) ShowReport {
	return ShowReport{
		SessionID:  stringFromAny(fields["session_id"]),
		SessionDir: stringFromAny(fields["session_dir"]),
		Fields:     cloneMap(fields),
	}
}

func NewHealthReport(fields map[string]any) HealthReport {
	return HealthReport{
		Scope:     stringFromAny(fields["scope"]),
		Status:    stringFromAny(fields["status"]),
		SessionID: stringFromAny(fields["session_id"]),
		Fields:    cloneMap(fields),
	}
}

func NewExportReport(fields map[string]any) ExportReport {
	return ExportReport{
		SessionID: stringFromAny(fields["session_id"]),
		Fields:    cloneMap(fields),
	}
}

func (r ShowReport) ToMap() map[string]any   { return cloneMap(r.Fields) }
func (r HealthReport) ToMap() map[string]any { return cloneMap(r.Fields) }
func (r ExportReport) ToMap() map[string]any { return cloneMap(r.Fields) }
