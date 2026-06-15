package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultClaudeStallSeconds = 300

var defaultClaudePollInterval = 2 * time.Second

type claudeBackend struct {
	sessionRoot string
	slotID      string
	label       string
	cwd         string
	profileID   string
	model       string
	effort      string
	sessionID   string
	started     bool
}

type claudeProcessResult struct {
	Stdout               string
	Stderr               string
	ReturnCode           int
	TimedOut             bool
	Stalled              bool
	JSONLActiveAtTimeout bool
	RecoverySource       string
}

func newClaudeBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) *claudeBackend {
	if cwd == "" {
		cwd = sessionRoot
	}
	sessionID, err := newUUID()
	if err != nil {
		sessionID = fmt.Sprintf("relay-%d", time.Now().UnixNano())
	}
	return &claudeBackend{
		sessionRoot: sessionRoot,
		slotID:      slotID,
		label:       label,
		cwd:         cwd,
		profileID:   config.ProfileID,
		model:       config.Model,
		effort:      config.Effort,
		sessionID:   sessionID,
	}
}

func (b *claudeBackend) Name() string {
	return "claude"
}

func (b *claudeBackend) SlotID() string {
	return b.slotID
}

func (b *claudeBackend) Label() string {
	return b.label
}

func (b *claudeBackend) RunTurn(ctx context.Context, prompt string, options TurnOptions) (TurnResult, error) {
	stallTimeout := options.StallTimeoutSeconds
	if stallTimeout <= 0 {
		stallTimeout = defaultClaudeStallSeconds
	}
	result, response, err := b.attemptTurn(ctx, prompt, options.TimeoutSeconds, stallTimeout)
	if err != nil {
		return TurnResult{}, err
	}
	if !b.started && result.ReturnCode != 0 && response == "" && claudeSessionIDInUse(result) {
		sessionID, uuidErr := newUUID()
		if uuidErr != nil {
			return TurnResult{}, uuidErr
		}
		b.sessionID = sessionID
		result, response, err = b.attemptTurn(ctx, prompt, options.TimeoutSeconds, stallTimeout)
		if err != nil {
			return TurnResult{}, err
		}
	}

	providerResult := ProviderResult{
		Backend:         "claude",
		TimedOut:        result.TimedOut,
		Stalled:         result.Stalled,
		Recovered:       false,
		ReturnCode:      result.ReturnCode,
		ReturnCodeKnown: true,
		RecoverySource:  "",
		Warnings:        claudeWarnings(result, options.TimeoutSeconds, stallTimeout),
	}

	if result.ReturnCode != 0 && response == "" && !result.TimedOut && !result.Stalled {
		detail := claudeErrorText(result)
		if retryableError := classifyRetryableProviderError(detail); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		}
		return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: detail}
	}
	if result.ReturnCode != 0 && response != "" {
		if providerFailureCategory(response) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: response}
		}
		providerResult.Recovered = true
		providerResult.RecoverySource = result.RecoverySource
	}
	if response != "" && (result.TimedOut || result.Stalled || result.ReturnCode != 0 || result.RecoverySource == "stdout") {
		if retryableError := classifyRetryableProviderError(response); retryableError != "" {
			providerResult.RetryableError = retryableError
			return TurnResult{ProviderResult: providerResult}, RetryableProviderError{Label: b.label, Detail: retryableError}
		}
		if providerFailureCategory(response) == "auth" {
			return TurnResult{ProviderResult: providerResult}, BackendRunError{Label: b.label, Detail: response}
		}
	}
	if (result.TimedOut || result.Stalled) && response != "" {
		providerResult.Recovered = true
		providerResult.RecoverySource = result.RecoverySource
	}

	if !(result.TimedOut || result.Stalled) || response != "" {
		b.started = true
	}
	content := response
	if content == "" {
		if result.Stalled {
			content = fmt.Sprintf("[%s stalled after %ds of no JSONL activity]", b.label, stallTimeout)
		} else if result.TimedOut {
			content = fmt.Sprintf("[%s timed out after %ds]", b.label, options.TimeoutSeconds)
		} else {
			content = fmt.Sprintf("[No response from %s]", b.label)
		}
	}
	return TurnResult{
		Content:        content,
		TimedOut:       providerResult.TimedOut,
		Stalled:        providerResult.Stalled,
		Recovered:      providerResult.Recovered,
		ProviderResult: providerResult,
	}, nil
}

func (b *claudeBackend) SessionState() map[string]any {
	state := map[string]any{
		"session_id": b.sessionID,
		"started":    b.started,
		"cwd":        b.cwd,
		"profile_id": nil,
		"model":      nil,
		"effort":     nil,
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

func (b *claudeBackend) RestoreState(state map[string]any, override SlotConfig) error {
	if state == nil {
		state = map[string]any{}
	}
	sessionID, ok := state["session_id"].(string)
	if !ok || sessionID == "" {
		return fmt.Errorf("session_id must be a non-empty string")
	}
	started, ok := state["started"].(bool)
	if state["started"] != nil && !ok {
		return fmt.Errorf("started must be a bool")
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
	b.sessionID = sessionID
	b.started = started || state["started"] == nil
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

func (b *claudeBackend) Cleanup() error {
	jsonlPath := b.jsonlPath()
	projectDir := filepath.Dir(jsonlPath)
	if err := os.Remove(jsonlPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	sessionDir := filepath.Join(projectDir, b.sessionID)
	if err := os.RemoveAll(sessionDir); err != nil {
		return err
	}
	if err := os.Remove(projectDir); err != nil && !os.IsNotExist(err) {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
	return nil
}

func (b *claudeBackend) attemptTurn(ctx context.Context, prompt string, timeoutSeconds int, stallTimeoutSeconds int) (claudeProcessResult, string, error) {
	jsonlPath := b.jsonlPath()
	preSize := fileSize(jsonlPath)
	result, err := b.runProcess(ctx, prompt, jsonlPath, timeoutSeconds, stallTimeoutSeconds, defaultClaudePollInterval)
	if err != nil {
		return result, "", err
	}
	response := extractClaudeResponse(jsonlPath, preSize)
	if response != "" {
		result.RecoverySource = "jsonl"
	} else if strings.TrimSpace(result.Stdout) != "" {
		response = strings.TrimSpace(result.Stdout)
		result.RecoverySource = "stdout"
	}
	return result, response, nil
}

func (b *claudeBackend) runProcess(ctx context.Context, prompt string, jsonlPath string, timeoutSeconds int, stallTimeoutSeconds int, pollInterval time.Duration) (claudeProcessResult, error) {
	command := b.buildCommand()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = b.cwd
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return claudeProcessResult{ReturnCode: -1}, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return claudeProcessResult{ReturnCode: -1}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return claudeProcessResult{ReturnCode: -1}, err
	}

	if err := cmd.Start(); err != nil {
		return claudeProcessResult{ReturnCode: -1}, err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(&stdout, stdoutPipe)
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(&stderr, stderrPipe)
	}()
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	go func() {
		_, _ = io.WriteString(stdin, prompt)
		_ = stdin.Close()
	}()

	result := monitorClaudeProcess(ctx, cmd, waitCh, jsonlPath, timeoutSeconds, stallTimeoutSeconds, pollInterval)
	copies.Wait()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if result.ReturnCode == 0 && cmd.ProcessState != nil {
		result.ReturnCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(ctx.Err(), context.Canceled) && !result.TimedOut && !result.Stalled {
		return result, ctx.Err()
	}
	return result, nil
}

func (b *claudeBackend) buildCommand() []string {
	command := []string{"claude", "-p", "--dangerously-skip-permissions"}
	if b.started {
		command = append(command, "--resume", b.sessionID)
	} else {
		command = append(command, "--session-id", b.sessionID)
	}
	if b.model != "" {
		command = append(command, "--model", b.model)
	}
	if b.effort != "" {
		command = append(command, "--effort", b.effort)
	}
	return append(command, "-")
}

func (b *claudeBackend) jsonlPath() string {
	return filepath.Join(claudeProjectDir(b.cwd), b.sessionID+".jsonl")
}

func monitorClaudeProcess(ctx context.Context, cmd *exec.Cmd, waitCh <-chan error, jsonlPath string, timeoutSeconds int, stallTimeoutSeconds int, pollInterval time.Duration) claudeProcessResult {
	if pollInterval <= 0 {
		pollInterval = defaultClaudePollInterval
	}
	initialSize := jsonlTotalSize(jsonlPath)
	lastSize := initialSize
	startedAt := time.Now()
	lastActivityAt := startedAt
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case err := <-waitCh:
			return claudeProcessResult{ReturnCode: processExitCode(cmd, err)}
		case <-ctx.Done():
			killProcess(cmd)
			err := <-waitCh
			return claudeProcessResult{ReturnCode: processExitCode(cmd, err)}
		case now := <-ticker.C:
			currentSize := jsonlTotalSize(jsonlPath)
			if currentSize > lastSize {
				lastSize = currentSize
				lastActivityAt = now
			}
			if timeoutSeconds > 0 && now.Sub(startedAt) >= time.Duration(timeoutSeconds)*time.Second {
				killProcess(cmd)
				err := <-waitCh
				return claudeProcessResult{
					ReturnCode:           processExitCode(cmd, err),
					TimedOut:             true,
					JSONLActiveAtTimeout: currentSize > initialSize && now.Sub(lastActivityAt) < time.Duration(stallTimeoutSeconds)*time.Second,
				}
			}
			if stallTimeoutSeconds > 0 && now.Sub(lastActivityAt) >= time.Duration(stallTimeoutSeconds)*time.Second {
				killProcess(cmd)
				err := <-waitCh
				return claudeProcessResult{
					ReturnCode: processExitCode(cmd, err),
					Stalled:    true,
				}
			}
		}
	}
}

func claudeProjectDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	var encoded strings.Builder
	for _, ch := range cwd {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' {
			encoded.WriteRune(ch)
		} else {
			encoded.WriteByte('-')
		}
	}
	return filepath.Join(home, ".claude", "projects", encoded.String())
}

func jsonlTotalSize(jsonlPath string) int64 {
	var total int64
	if stat, err := os.Stat(jsonlPath); err == nil {
		total += stat.Size()
	}
	subagentsDir := filepath.Join(filepath.Dir(jsonlPath), strings.TrimSuffix(filepath.Base(jsonlPath), filepath.Ext(jsonlPath)), "subagents")
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return total
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if stat, err := os.Stat(filepath.Join(subagentsDir, entry.Name())); err == nil {
			total += stat.Size()
		}
	}
	return total
}

func extractClaudeResponse(jsonlPath string, afterByte int64) string {
	file, err := os.Open(jsonlPath)
	if err != nil {
		return ""
	}
	defer file.Close()
	if afterByte > 0 {
		if _, err := file.Seek(afterByte, io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	var parts []string
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["type"] != "assistant" {
			continue
		}
		message, _ := record["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			if block["type"] != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func claudeErrorText(result claudeProcessResult) string {
	raw := strings.TrimSpace(result.Stderr)
	if raw == "" {
		raw = strings.TrimSpace(result.Stdout)
	}
	if raw == "" {
		return fmt.Sprintf("process exited %d", result.ReturnCode)
	}
	return collapseWhitespace(raw)
}

func claudeSessionIDInUse(result claudeProcessResult) bool {
	combined := strings.ToLower(result.Stderr + "\n" + result.Stdout)
	return strings.Contains(combined, "session id") && strings.Contains(combined, "already in use")
}

func claudeWarnings(result claudeProcessResult, timeoutSeconds int, stallTimeoutSeconds int) []string {
	var warnings []string
	if result.Stalled {
		warnings = append(warnings, fmt.Sprintf("claude stalled - no JSONL activity for %ds, killing process", stallTimeoutSeconds))
	} else if result.TimedOut && result.JSONLActiveAtTimeout {
		warnings = append(warnings, fmt.Sprintf("claude hit hard timeout after %ds (JSONL was still growing - working but slow)", timeoutSeconds))
	} else if result.TimedOut {
		warnings = append(warnings, fmt.Sprintf("claude hit hard timeout after %ds", timeoutSeconds))
	} else if result.ReturnCode != 0 {
		warnings = append(warnings, fmt.Sprintf("claude exited %d", result.ReturnCode))
	}
	if result.RecoverySource != "" && (result.Stalled || result.TimedOut) {
		warnings = append(warnings, "claude response recovered from "+result.RecoverySource)
	}
	if result.ReturnCode != 0 && result.Stderr != "" {
		warnings = append(warnings, truncateString(result.Stderr, 500))
	}
	return warnings
}

func fileSize(path string) int64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return stat.Size()
}

func processExitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	hexText := hex.EncodeToString(data[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexText[0:8], hexText[8:12], hexText[12:16], hexText[16:20], hexText[20:32]), nil
}
