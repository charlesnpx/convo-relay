package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charlesnpx/convo-relay/internal/model"
)

const (
	retryInitialBackoffSeconds = 5
	retryMaxBackoffSeconds     = 160
)

var retryBackoff = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

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

type ProviderFailureError struct {
	Label     string
	Detail    string
	Category  string
	Retryable bool
	Attempts  int
	Cause     error
}

func (e ProviderFailureError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" && e.Cause != nil {
		detail = e.Cause.Error()
	}
	if detail == "" {
		detail = "provider failure"
	}
	if strings.TrimSpace(e.Label) == "" {
		return detail
	}
	return fmt.Sprintf("%s failed: %s", e.Label, detail)
}

func (e ProviderFailureError) Unwrap() error {
	return e.Cause
}

type ProviderResult = model.ProviderResult

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

func providerResultMap(result ProviderResult) map[string]any {
	return result.ToMap()
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

func runWithRetryableProviderErrors[T any](ctx context.Context, label string, operation func() (T, error)) (T, error) {
	delay := retryInitialBackoffSeconds
	attempts := 0
	for {
		attempts++
		result, err := operation()
		if err == nil {
			return result, nil
		}
		var retryable RetryableProviderError
		if !errors.As(err, &retryable) {
			return result, err
		}
		if delay > retryMaxBackoffSeconds {
			detail := fmt.Sprintf("failed after retryable provider errors: %s", firstNonEmpty(retryable.Detail, err.Error()))
			return result, ProviderFailureError{
				Label:     label,
				Detail:    detail,
				Category:  "transient",
				Retryable: true,
				Attempts:  attempts,
				Cause: BackendRunError{
					Label:  label,
					Detail: detail,
				},
			}
		}
		if backoffErr := retryBackoff(ctx, time.Duration(delay)*time.Second); backoffErr != nil {
			return result, backoffErr
		}
		delay *= 2
	}
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

func providerFailurePayload(phase string, actor string, backend string, err error, result ProviderResult) map[string]any {
	detail := providerFailureDetail(err, result)
	category := providerFailureCategory(detail)
	retryable := false
	attempts := 1
	var retryableErr RetryableProviderError
	if errors.As(err, &retryableErr) {
		retryable = true
	}
	var failureErr ProviderFailureError
	if errors.As(err, &failureErr) {
		if failureErr.Category != "" {
			category = failureErr.Category
		}
		retryable = failureErr.Retryable
		if failureErr.Attempts > 0 {
			attempts = failureErr.Attempts
		}
	}
	return map[string]any{
		"phase":             phase,
		"actor":             actor,
		"backend":           backend,
		"category":          category,
		"retryable":         retryable,
		"attempts":          attempts,
		"timed_out":         result.TimedOut,
		"stalled":           result.Stalled,
		"return_code":       providerReturnCodeForFailure(result),
		"remediation_code":  providerRemediationCode(category, backend),
		"remediation":       providerRemediationText(category, backend),
		"sanitized_detail":  sanitizeProviderFailureDetail(detail),
		"raw_detail_hidden": true,
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
	var failureErr ProviderFailureError
	if errors.As(err, &failureErr) && strings.TrimSpace(failureErr.Detail) != "" {
		return failureErr.Detail
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
	replacers := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_ -]?key|token|authorization|bearer)\s*[:=]\s*['"]?[^'"\s]+`),
	}
	for _, replacer := range replacers {
		cleaned = replacer.ReplaceAllString(cleaned, "$1=[redacted]")
	}
	cleaned = regexp.MustCompile(`(?i)sk-[a-z0-9_-]{8,}`).ReplaceAllString(cleaned, "[redacted-key]")
	return truncateString(cleaned, 500)
}
