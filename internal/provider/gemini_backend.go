package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type geminiBackend struct {
	sessionRoot     string
	slotID          string
	label           string
	cwd             string
	profileID       string
	model           string
	effort          string
	started         bool
	sessionRef      string
	geminiHome      string
	geminiHomeReady bool
}

func newGeminiBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *geminiBackend {
	if cwd == "" {
		cwd = sessionRoot
	}
	return &geminiBackend{
		sessionRoot: sessionRoot,
		slotID:      slotID,
		label:       label,
		cwd:         cwd,
		profileID:   config.ProfileID,
		model:       config.Model,
		effort:      config.Effort,
		geminiHome:  filepath.Join(sessionRoot, "gemini", slotID),
	}
}

func (b *geminiBackend) Name() string {
	return "gemini"
}

func (b *geminiBackend) SlotID() string {
	return b.slotID
}

func (b *geminiBackend) Label() string {
	return b.label
}

func (b *geminiBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	if err := b.ensureGeminiHome(); err != nil {
		return TurnResult{}, err
	}
	result, err := runSubprocess(ctx, b.buildCommand(), prompt, b.cwd, b.env(), options.TimeoutSeconds)
	providerResult := newProviderResult("gemini", result)
	text, sessionRef := parseGeminiOutput(result.Stdout)
	if sessionRef != "" {
		b.sessionRef = sessionRef
	} else if b.sessionRef == "" && text != "" {
		b.sessionRef = "latest"
	}
	if errors.Is(err, context.Canceled) {
		return TurnResult{}, err
	}
	if err != nil {
		return TurnResult{}, err
	}
	if result.TimedOut {
		if text == "" {
			providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("gemini timed out after %ds with no recoverable response", options.TimeoutSeconds))
			text = fmt.Sprintf("[%s timed out after %ds]", b.label, options.TimeoutSeconds)
		} else if retryableError := classifyRetryableProviderError(text); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		} else if providerFailureCategory(text) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: text}
		} else {
			providerResult.Recovered = true
			providerResult.RecoverySource = "output"
			providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("gemini timed out after %ds but response recovered from output", options.TimeoutSeconds))
		}
	} else if result.ReturnCode != 0 {
		if text == "" {
			detail := strings.TrimSpace(result.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(result.Stdout)
			}
			if detail == "" {
				detail = fmt.Sprintf("process exited %d", result.ReturnCode)
			}
			detail = collapseWhitespace(detail)
			if retryableError := classifyRetryableProviderError(detail); retryableError != "" {
				providerResult.RetryableError = retryableError
				return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
			}
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: detail}
		}
		if retryableError := classifyRetryableProviderError(text); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		}
		if providerFailureCategory(text) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: text}
		}
		providerResult.Recovered = true
		providerResult.RecoverySource = "output"
		providerResult.Warnings = append(providerResult.Warnings, fmt.Sprintf("gemini exited %d", result.ReturnCode))
		if result.Stderr != "" {
			providerResult.Warnings = append(providerResult.Warnings, truncateString(result.Stderr, 500))
		}
	}
	if !result.TimedOut || text != "" {
		b.started = true
	}
	if text == "" {
		text = fmt.Sprintf("[No response from %s]", b.label)
	}
	return TurnResult{
		Content:        text,
		TimedOut:       providerResult.TimedOut,
		Stalled:        providerResult.Stalled,
		Recovered:      providerResult.Recovered,
		ProviderResult: providerResult,
	}, nil
}

func (b *geminiBackend) SessionState() SlotState {
	state := map[string]any{
		"session_ref": b.sessionRef,
		"started":     b.started,
		"cwd":         b.cwd,
		"profile_id":  nil,
		"model":       nil,
		"effort":      nil,
	}
	if b.profileID != "" {
		state["profile_id"] = b.profileID
	}
	if b.model != "" {
		state["model"] = b.model
	}
	if b.effort != "" {
		state["effort"] = b.effort
	}
	return state
}

func (b *geminiBackend) RestoreState(state SlotState, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	started, ok := state["started"].(bool)
	if state["started"] != nil && !ok {
		return fmt.Errorf("started must be a bool")
	}
	sessionRef, ok := state["session_ref"].(string)
	if state["session_ref"] != nil && (!ok || sessionRef == "") {
		return fmt.Errorf("session_ref must be a non-empty string")
	}
	cwd, ok := state["cwd"].(string)
	if state["cwd"] != nil && !ok {
		return fmt.Errorf("cwd must be a string")
	}
	profileID, ok := state["profile_id"].(string)
	if state["profile_id"] != nil && !ok {
		return fmt.Errorf("profile_id must be a string")
	}
	model, ok := state["model"].(string)
	if state["model"] != nil && !ok {
		return fmt.Errorf("model must be a string")
	}
	effort, ok := state["effort"].(string)
	if state["effort"] != nil && !ok {
		return fmt.Errorf("effort must be a string")
	}
	b.sessionRef = sessionRef
	b.started = started || sessionRef != ""
	if cwd != "" {
		b.cwd = cwd
	}
	if override.ProfileID != "" {
		b.profileID = override.ProfileID
	} else if profileID != "" {
		b.profileID = profileID
	}
	if override.Model != "" {
		b.model = override.Model
	} else if model != "" {
		b.model = model
	}
	if override.Effort != "" {
		b.effort = override.Effort
	} else if effort != "" {
		b.effort = effort
	}
	return nil
}

func (b *geminiBackend) Cleanup() error {
	return nil
}

func (b *geminiBackend) buildCommand() []string {
	command := []string{"gemini", "--output-format", "json"}
	if b.model != "" {
		command = append(command, "--model", b.model)
	}
	return command
}

func (b *geminiBackend) env() []string {
	env := os.Environ()
	env = append(env, "HOME="+b.geminiHome, "GEMINI_CLI_HOME="+b.geminiHome)
	return env
}

func (b *geminiBackend) ensureGeminiHome() error {
	if b.geminiHomeReady {
		return nil
	}
	geminiConfig := filepath.Join(b.geminiHome, ".gemini")
	if err := os.MkdirAll(geminiConfig, 0o755); err != nil {
		return err
	}
	homeDir, err := os.UserHomeDir()
	if err == nil {
		realConfig := filepath.Join(homeDir, ".gemini")
		entries, readErr := os.ReadDir(realConfig)
		if readErr == nil {
			for _, entry := range entries {
				if entry.Name() == "tmp" || entry.Name() == "logs" {
					continue
				}
				src := filepath.Join(realConfig, entry.Name())
				dst := filepath.Join(geminiConfig, entry.Name())
				if _, err := os.Lstat(dst); os.IsNotExist(err) {
					_ = os.Symlink(src, dst)
				}
			}
		}
	}
	b.geminiHomeReady = true
	return nil
}

func parseGeminiOutput(stdout string) (string, string) {
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return "", ""
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return raw, ""
	}
	for _, key := range []string{"response", "text", "content", "message"} {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), geminiSessionRef(data)
		}
	}
	if errorObject, ok := data["error"].(map[string]any); ok {
		if message, ok := errorObject["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message), geminiSessionRef(data)
		}
	}
	if errorText, ok := data["error"].(string); ok && strings.TrimSpace(errorText) != "" {
		return strings.TrimSpace(errorText), geminiSessionRef(data)
	}
	return raw, ""
}

func geminiSessionRef(data map[string]any) string {
	value, ok := data["session_id"].(string)
	if !ok || value == "" {
		return ""
	}
	return value
}
