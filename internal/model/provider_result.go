package model

// ProviderResult carries the runtime observations collected by provider
// implementations for retry classification and user-facing diagnostics.
type ProviderResult struct {
	Backend         string
	TimedOut        bool
	Stalled         bool
	Recovered       bool
	ReturnCode      int
	ReturnCodeKnown bool
	RecoverySource  string
	Warnings        []string
	RetryableError  string
	Extra           map[string]any
}
