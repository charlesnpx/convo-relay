package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	StatusNotInstalled         = "not_installed"
	StatusInstalledAuthUnknown = "installed_auth_unknown"
	StatusReady                = "ready"
	StatusAuthFailed           = "auth_failed"
	StatusProbeFailed          = "probe_failed"
	StatusUnsupportedProbe     = "unsupported_probe"

	AuthenticationUnknown       = "unknown"
	AuthenticationAuthenticated = "authenticated"
	AuthenticationFailed        = "unauthenticated"
	AuthenticationUnsupported   = "unsupported"
	AuthenticationProbeFailed   = "probe_failed"

	ProbeStatusNotRun       = "not_run"
	ProbeStatusPassed       = "passed"
	ProbeStatusFailed       = "failed"
	ProbeStatusTimedOut     = "timed_out"
	ProbeStatusNotInstalled = "not_installed"
	ProbeStatusUnsupported  = "unsupported"

	DefaultProbeTimeout = 5 * time.Second
	maxProbeOutputBytes = 64 * 1024
	probeWaitDelay      = 250 * time.Millisecond
)

var registeredBackends = []string{"claude", "codex", "gemini"}

type Record struct {
	Backend              string      `json:"backend"`
	ExecutablePath       string      `json:"executable_path"`
	Version              string      `json:"version"`
	AuthenticationStatus string      `json:"authentication_status"`
	Status               string      `json:"status"`
	ProbeDetail          ProbeDetail `json:"probe_detail"`
}

type ProbeDetail struct {
	Installation   Probe `json:"installation"`
	Version        Probe `json:"version"`
	Authentication Probe `json:"authentication"`
}

type Probe struct {
	Supported       bool     `json:"supported"`
	Attempted       bool     `json:"attempted"`
	Command         []string `json:"command"`
	Status          string   `json:"status"`
	ExitCode        *int     `json:"exit_code"`
	Output          string   `json:"output"`
	OutputTruncated bool     `json:"output_truncated"`
	Error           string   `json:"error"`
}

type Report struct {
	Scope     string   `json:"scope"`
	ProbeAuth bool     `json:"probe_auth"`
	Backends  []Record `json:"backends"`
}

type Options struct {
	ProbeAuth bool
	Timeout   time.Duration
	LookPath  func(string) (string, error)
}

type backendSpec struct {
	name        string
	versionArgs []string
	authArgs    []string
	parseAuth   func(commandResult) (string, bool)
}

type commandResult struct {
	output    string
	stdout    string
	stderr    string
	exitCode  *int
	err       error
	timedOut  bool
	truncated bool
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func CheckRegistered(ctx context.Context, options Options) Report {
	records, _ := Check(ctx, registeredBackends, options)
	return Report{Scope: "backends", ProbeAuth: options.ProbeAuth, Backends: records}
}

func Check(ctx context.Context, backends []string, options Options) ([]Record, error) {
	options = effectiveOptions(options)
	names, err := normalizeBackends(backends)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(names))
	for _, name := range names {
		records = append(records, checkBackend(ctx, backendSpecification(name), options))
	}
	return records, nil
}

func FormatReport(report Report) string {
	lines := []string{"Backend readiness:"}
	for _, record := range report.Backends {
		executable := record.ExecutablePath
		if executable == "" {
			executable = "-"
		}
		version := record.Version
		if version == "" {
			version = "-"
		}
		lines = append(lines, fmt.Sprintf(
			"  %-7s %-22s auth=%-14s version=%q executable=%s",
			record.Backend,
			record.Status,
			record.AuthenticationStatus,
			version,
			executable,
		))
		if detail := firstProbeFailure(record); detail != "" {
			lines = append(lines, "    "+detail)
		}
	}
	return strings.Join(lines, "\n")
}

func checkBackend(ctx context.Context, spec backendSpec, options Options) Record {
	record := Record{
		Backend:              spec.name,
		AuthenticationStatus: AuthenticationUnknown,
		Status:               StatusNotInstalled,
		ProbeDetail: ProbeDetail{
			Installation:   newProbe(true),
			Version:        newProbe(true),
			Authentication: newProbe(len(spec.authArgs) > 0),
		},
	}
	record.ProbeDetail.Installation.Attempted = true
	path, err := options.LookPath(spec.name)
	if err != nil {
		record.ProbeDetail.Installation.Status = ProbeStatusNotInstalled
		record.ProbeDetail.Installation.Error = strings.TrimSpace(err.Error())
		return record
	}
	if absolute, absoluteErr := filepath.Abs(path); absoluteErr == nil {
		path = absolute
	}
	record.ExecutablePath = filepath.Clean(path)
	record.ProbeDetail.Installation.Status = ProbeStatusPassed

	versionResult := runProbeCommand(ctx, options.Timeout, record.ExecutablePath, spec.versionArgs...)
	record.ProbeDetail.Version = probeFromResult(spec.name, spec.versionArgs, versionResult)
	if !commandCompletedSuccessfully(versionResult) || strings.TrimSpace(versionResult.output) == "" {
		if record.ProbeDetail.Version.Error == "" && strings.TrimSpace(versionResult.output) == "" {
			record.ProbeDetail.Version.Error = "version probe returned empty output"
		}
		if record.ProbeDetail.Version.Status == ProbeStatusPassed {
			record.ProbeDetail.Version.Status = ProbeStatusFailed
		}
		record.Status = StatusProbeFailed
		return record
	}
	record.Version = strings.TrimSpace(versionResult.output)
	record.Status = StatusInstalledAuthUnknown
	if !options.ProbeAuth {
		return record
	}
	if len(spec.authArgs) == 0 {
		record.AuthenticationStatus = AuthenticationUnsupported
		record.Status = StatusUnsupportedProbe
		record.ProbeDetail.Authentication.Attempted = false
		record.ProbeDetail.Authentication.Status = ProbeStatusUnsupported
		return record
	}

	authResult := runProbeCommand(ctx, options.Timeout, record.ExecutablePath, spec.authArgs...)
	record.ProbeDetail.Authentication = probeFromResult(spec.name, spec.authArgs, authResult)
	authentication, recognized := spec.parseAuth(authResult)
	if !authResult.timedOut && recognized && authentication == AuthenticationFailed {
		record.AuthenticationStatus = AuthenticationFailed
		record.Status = StatusAuthFailed
		return record
	}
	if recognized && authentication == AuthenticationAuthenticated && commandCompletedSuccessfully(authResult) {
		record.AuthenticationStatus = AuthenticationAuthenticated
		record.Status = StatusReady
		return record
	}
	record.AuthenticationStatus = AuthenticationProbeFailed
	record.Status = StatusProbeFailed
	if record.ProbeDetail.Authentication.Error == "" {
		record.ProbeDetail.Authentication.Error = "authentication probe returned an unrecognized result"
	}
	if record.ProbeDetail.Authentication.Status == ProbeStatusPassed {
		record.ProbeDetail.Authentication.Status = ProbeStatusFailed
	}
	return record
}

func effectiveOptions(options Options) Options {
	if options.Timeout <= 0 {
		options.Timeout = DefaultProbeTimeout
	}
	if options.LookPath == nil {
		options.LookPath = exec.LookPath
	}
	return options
}

func normalizeBackends(backends []string) ([]string, error) {
	seen := map[string]bool{}
	for _, backend := range backends {
		name := strings.TrimSpace(backend)
		if !isRegistered(name) {
			return nil, fmt.Errorf("unknown backend %q", name)
		}
		seen[name] = true
	}
	result := make([]string, 0, len(seen))
	for _, name := range registeredBackends {
		if seen[name] {
			result = append(result, name)
		}
	}
	return result, nil
}

func backendSpecification(name string) backendSpec {
	switch name {
	case "codex":
		return backendSpec{name: name, versionArgs: []string{"--version"}, authArgs: []string{"login", "status"}, parseAuth: parseCodexAuth}
	case "claude":
		return backendSpec{name: name, versionArgs: []string{"--version"}, authArgs: []string{"auth", "status", "--json"}, parseAuth: parseClaudeAuth}
	case "gemini":
		return backendSpec{name: name, versionArgs: []string{"--version"}}
	default:
		panic("backendSpecification called with an unregistered backend")
	}
}

func newProbe(supported bool) Probe {
	return Probe{
		Supported: supported,
		Command:   []string{},
		Status:    ProbeStatusNotRun,
	}
}

func runProbeCommand(parent context.Context, timeout time.Duration, path string, args ...string) commandResult {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return runCommand(ctx, path, args...)
}

func runCommand(ctx context.Context, path string, args ...string) commandResult {
	cmd := exec.CommandContext(ctx, path, args...)
	var interrupted atomic.Bool
	configureProbeCommand(cmd, func() { interrupted.Store(true) })
	stdout := &limitedBuffer{remaining: maxProbeOutputBytes}
	stderr := &limitedBuffer{remaining: maxProbeOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	stdoutText := strings.TrimSpace(stdout.buffer.String())
	stderrText := strings.TrimSpace(stderr.buffer.String())
	output, outputTruncated := combineProbeOutput(stdoutText, stderrText)
	result := commandResult{
		output:    output,
		stdout:    stdoutText,
		stderr:    stderrText,
		err:       err,
		timedOut:  interrupted.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded),
		truncated: stdout.truncated || stderr.truncated || outputTruncated,
	}
	if cmd.ProcessState != nil {
		exitCode := cmd.ProcessState.ExitCode()
		result.exitCode = &exitCode
	}
	return result
}

func probeFromResult(name string, args []string, result commandResult) Probe {
	probe := newProbe(true)
	probe.Attempted = true
	probe.Command = append([]string{name}, args...)
	probe.ExitCode = result.exitCode
	probe.Output = result.output
	probe.OutputTruncated = result.truncated
	switch {
	case result.timedOut:
		probe.Status = ProbeStatusTimedOut
		probe.Error = "probe timed out"
	case result.err != nil:
		probe.Status = ProbeStatusFailed
		probe.Error = strings.TrimSpace(result.err.Error())
	default:
		probe.Status = ProbeStatusPassed
	}
	return probe
}

func commandCompletedSuccessfully(result commandResult) bool {
	return !result.timedOut && result.err == nil && result.exitCode != nil && *result.exitCode == 0
}

func parseCodexAuth(result commandResult) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(result.output))
	for _, marker := range []string{"not logged in", "logged out", "unauthenticated", "not authenticated"} {
		if strings.Contains(value, marker) {
			return AuthenticationFailed, true
		}
	}
	for _, marker := range []string{"logged in", "authenticated"} {
		if strings.Contains(value, marker) {
			return AuthenticationAuthenticated, true
		}
	}
	return "", false
}

func parseClaudeAuth(result commandResult) (string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		return "", false
	}
	for _, key := range []string{"loggedIn", "authenticated", "isAuthenticated"} {
		if value, exists := payload[key]; exists {
			authenticated, ok := value.(bool)
			if !ok {
				return "", false
			}
			if authenticated {
				return AuthenticationAuthenticated, true
			}
			return AuthenticationFailed, true
		}
	}
	return "", false
}

func combineProbeOutput(stdout string, stderr string) (string, bool) {
	output := &limitedBuffer{remaining: maxProbeOutputBytes}
	if stdout != "" {
		_, _ = output.Write([]byte(stdout))
	}
	if stdout != "" && stderr != "" {
		_, _ = output.Write([]byte("\n"))
	}
	if stderr != "" {
		_, _ = output.Write([]byte(stderr))
	}
	return strings.TrimSpace(output.buffer.String()), output.truncated
}

func firstProbeFailure(record Record) string {
	probes := []Probe{record.ProbeDetail.Installation, record.ProbeDetail.Version, record.ProbeDetail.Authentication}
	for _, probe := range probes {
		if probe.Error != "" {
			return probe.Error
		}
	}
	return ""
}

func isRegistered(name string) bool {
	index := sort.SearchStrings(registeredBackends, name)
	return index < len(registeredBackends) && registeredBackends[index] == name
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	if b.remaining <= 0 {
		b.truncated = b.truncated || len(data) > 0
		return written, nil
	}
	accepted := len(data)
	if accepted > b.remaining {
		accepted = b.remaining
		b.truncated = true
	}
	_, _ = b.buffer.Write(data[:accepted])
	b.remaining -= accepted
	return written, nil
}
