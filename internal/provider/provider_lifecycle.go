package provider

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type BackendRunError struct {
	Label  string
	Detail string
}

func (e BackendRunError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = "unknown error"
	}
	if strings.TrimSpace(e.Label) == "" {
		return detail
	}
	return fmt.Sprintf("%s failed: %s", e.Label, detail)
}

type RetryableProviderError struct {
	Label  string
	Detail string
}

func (e RetryableProviderError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = "retryable provider error"
	}
	if strings.TrimSpace(e.Label) == "" {
		return detail
	}
	return fmt.Sprintf("%s failed: %s", e.Label, detail)
}

// ProviderFailure is the typed failure record that runner serializes into its
// legacy event payload.
type ProviderFailure struct {
	Phase           string
	Actor           string
	Backend         string
	Category        string
	Retryable       bool
	Attempts        int
	TimedOut        bool
	Stalled         bool
	ReturnCode      any
	RemediationCode string
	Remediation     string
	SanitizedDetail string
	RawDetailHidden bool
}

// ProviderResult carries runtime observations used for retry classification
// and user-facing diagnostics.
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

func newProviderResult(backend string, result processResult) ProviderResult {
	return ProviderResult{
		Backend:         backend,
		TimedOut:        result.TimedOut,
		ReturnCode:      result.ReturnCode,
		ReturnCodeKnown: true,
		Warnings:        []string{},
	}
}

func providerResultForTurn(backend string, result TurnResult) ProviderResult {
	providerResult := result.ProviderResult
	if providerResult.Backend == "" {
		providerResult.Backend = backend
	}
	providerResult.TimedOut = providerResult.TimedOut || result.TimedOut
	providerResult.Stalled = providerResult.Stalled || result.Stalled
	providerResult.Recovered = providerResult.Recovered || result.Recovered
	if providerResult.Warnings == nil {
		providerResult.Warnings = []string{}
	}
	return providerResult
}

func ProviderResultForTurn(backend string, result TurnResult) ProviderResult {
	return providerResultForTurn(backend, result)
}

var nonRetryableProviderErrorHints = []string{
	"command not found",
	"no such file or directory",
	"not a git repository",
	"failed to parse",
	"invalid value",
	"unknown option",
	"session id",
	"already in use",
	"missing",
	"not installed",
}

var authProviderErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bauthentication error\b`),
	regexp.MustCompile(`\bunauthorized\b`),
	regexp.MustCompile(`\bforbidden\b`),
	regexp.MustCompile(`\binvalid api key\b`),
	regexp.MustCompile(`\binvalid token\b`),
	regexp.MustCompile(`\btoken expired\b`),
	regexp.MustCompile(`\blogin required\b`),
}

var retryableProviderErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bapi error\b`),
	regexp.MustCompile(`\brate limit\b`),
	regexp.MustCompile(`\btoo many requests\b`),
	regexp.MustCompile(`\btemporarily unavailable\b`),
	regexp.MustCompile(`\bservice unavailable\b`),
	regexp.MustCompile(`\bbad gateway\b`),
	regexp.MustCompile(`\bgateway timeout\b`),
	regexp.MustCompile(`\binternal server error\b`),
	regexp.MustCompile(`\boverloaded\b`),
	regexp.MustCompile(`\bupstream error\b`),
	regexp.MustCompile(`\bnetwork error\b`),
	regexp.MustCompile(`\bconnection reset\b`),
	regexp.MustCompile(`\beconnreset\b`),
	regexp.MustCompile(`\betimedout\b`),
	regexp.MustCompile(`\b(?:http|status)(?:\s+code)?[:= ]+(?:429|5\d\d)\b`),
}

func classifyRetryableProviderError(text string) string {
	cleaned := collapseWhitespace(text)
	if cleaned == "" {
		return ""
	}
	lowered := strings.ToLower(cleaned)
	for _, hint := range nonRetryableProviderErrorHints {
		if strings.Contains(lowered, hint) {
			return ""
		}
	}
	if providerFailureCategory(cleaned) == "auth" {
		return ""
	}
	for _, pattern := range retryableProviderErrorPatterns {
		if pattern.MatchString(lowered) {
			return cleaned
		}
	}
	return ""
}

func providerFailureCategory(text string) string {
	cleaned := collapseWhitespace(text)
	if cleaned == "" {
		return "unknown"
	}
	lowered := strings.ToLower(cleaned)
	for _, pattern := range authProviderErrorPatterns {
		if pattern.MatchString(lowered) {
			return "auth"
		}
	}
	for _, hint := range nonRetryableProviderErrorHints {
		if strings.Contains(lowered, hint) {
			return "configuration"
		}
	}
	for _, pattern := range retryableProviderErrorPatterns {
		if pattern.MatchString(lowered) {
			return "transient"
		}
	}
	return "provider_error"
}

// NewProviderFailure classifies and sanitizes a provider failure without
// depending on runner's event-map representation.
func NewProviderFailure(phase string, actor string, backend string, err error, result ProviderResult) ProviderFailure {
	detail := providerFailureDetail(err, result)
	category := providerFailureCategory(detail)
	retryable := false
	attempts := 1
	var retryableErr RetryableProviderError
	if errors.As(err, &retryableErr) {
		retryable = true
	}
	return ProviderFailure{
		Phase:           phase,
		Actor:           actor,
		Backend:         backend,
		Category:        category,
		Retryable:       retryable,
		Attempts:        attempts,
		TimedOut:        result.TimedOut,
		Stalled:         result.Stalled,
		ReturnCode:      providerReturnCodeForFailure(result),
		RemediationCode: providerRemediationCode(category, backend),
		Remediation:     providerRemediationText(category, backend),
		SanitizedDetail: sanitizeProviderFailureDetail(detail),
		RawDetailHidden: true,
	}
}

func providerFailureDetail(err error, result ProviderResult) string {
	if strings.TrimSpace(result.RetryableError) != "" {
		return result.RetryableError
	}
	var retryableErr RetryableProviderError
	if errors.As(err, &retryableErr) && strings.TrimSpace(retryableErr.Detail) != "" {
		return retryableErr.Detail
	}
	var backendErr BackendRunError
	if errors.As(err, &backendErr) && strings.TrimSpace(backendErr.Detail) != "" {
		return backendErr.Detail
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func providerReturnCodeForFailure(result ProviderResult) any {
	if result.ReturnCodeKnown {
		return result.ReturnCode
	}
	return nil
}

func providerRemediationCode(category string, backend string) string {
	switch category {
	case "auth":
		return strings.TrimSpace(backend) + "_login"
	case "configuration":
		return "check_provider_installation"
	case "transient":
		return "retry_later"
	default:
		return "inspect_provider_setup"
	}
}

func providerRemediationText(category string, backend string) string {
	switch category {
	case "auth":
		switch strings.TrimSpace(backend) {
		case "codex":
			return "Refresh Codex CLI authentication and rerun the relay."
		case "claude":
			return "Run Claude Code login or check the active Claude credentials."
		case "gemini":
			return "Run Gemini CLI auth/login or check the configured Gemini credentials."
		default:
			return "Refresh provider authentication before retrying."
		}
	case "configuration":
		return "Check that the provider CLI is installed, on PATH, and accepts the configured flags."
	case "transient":
		return "Retry later or reduce provider load if the service remains unavailable."
	default:
		return "Inspect provider setup and rerun after correcting the failure."
	}
}

func sanitizeProviderFailureDetail(detail string) string {
	cleaned := collapseWhitespace(detail)
	if cleaned == "" {
		return ""
	}
	authorization := regexp.MustCompile(`(?i)(["']?authorization["']?\s*[:=]\s*["']?)(?:bearer\s+)?[^'"\s,;}]+`)
	cleaned = authorization.ReplaceAllString(cleaned, "${1}[redacted]")
	credentialField := regexp.MustCompile(`(?i)(["']?(?:api[_ -]?key|token)["']?\s*[:=]\s*["']?)[^'"\s,;}]+`)
	cleaned = credentialField.ReplaceAllString(cleaned, "${1}[redacted]")
	standaloneBearer := regexp.MustCompile(`(?i)\bbearer\s+["']?[^'"\s,;}]+`)
	cleaned = standaloneBearer.ReplaceAllString(cleaned, "Bearer [redacted]")
	cleaned = regexp.MustCompile(`(?i)sk-[a-z0-9_-]{8,}`).ReplaceAllString(cleaned, "[redacted-key]")
	return truncateString(cleaned, 500)
}

func SanitizeProviderFailureDetail(detail string) string {
	return sanitizeProviderFailureDetail(detail)
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
