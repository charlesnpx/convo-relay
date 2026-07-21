package readiness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultReadinessRunsOnlyVersionProbes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "probe.log")
	t.Setenv("READINESS_TEST_LOG", logPath)
	writeProbeExecutable(t, dir, "claude", successfulProbeScript("claude", `claude 2.1.0`, `{"loggedIn":true}`))
	writeProbeExecutable(t, dir, "codex", successfulProbeScript("codex", `codex-cli 1.2.3`, `Logged in using ChatGPT`))
	writeProbeExecutable(t, dir, "gemini", successfulProbeScript("gemini", `gemini 0.4.0`, ``))
	t.Setenv("PATH", dir)

	report := CheckRegistered(context.Background(), Options{})
	if report.Scope != "backends" || report.ProbeAuth || len(report.Backends) != 4 {
		t.Fatalf("report = %#v", report)
	}
	for _, record := range report.Backends[:3] {
		if record.Status != StatusInstalledAuthUnknown || record.AuthenticationStatus != AuthenticationUnknown {
			t.Fatalf("default record = %#v", record)
		}
		if !filepath.IsAbs(record.ExecutablePath) || record.Version == "" {
			t.Fatalf("path/version missing: %#v", record)
		}
		if record.ProbeDetail.Version.Command[1] != "--version" || record.ProbeDetail.Authentication.Attempted {
			t.Fatalf("unexpected default probes: %#v", record.ProbeDetail)
		}
	}
	relay := report.Backends[3]
	if relay.Backend != "relay" || relay.Status != StatusReady || relay.ExecutablePath != "built-in" || relay.AuthenticationStatus != AuthenticationNotApplicable {
		t.Fatalf("relay readiness = %#v", relay)
	}
	assertProbeLog(t, logPath, []string{"claude:--version", "codex:--version", "gemini:--version"})
}

func TestExplicitAuthenticationProbesNormalizeSupportedAndUnsupportedBackends(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "probe.log")
	t.Setenv("READINESS_TEST_LOG", logPath)
	writeProbeExecutable(t, dir, "claude", successfulProbeScript("claude", `claude 2.1.0`, `{"loggedIn":true,"authMethod":"test"}`))
	writeProbeExecutable(t, dir, "codex", successfulProbeScript("codex", `codex-cli 1.2.3`, `Logged in using ChatGPT`))
	writeProbeExecutable(t, dir, "gemini", successfulProbeScript("gemini", `gemini 0.4.0`, ``))
	t.Setenv("PATH", dir)

	report := CheckRegistered(context.Background(), Options{ProbeAuth: true})
	wantStatuses := map[string]string{
		"claude": StatusReady,
		"codex":  StatusReady,
		"gemini": StatusUnsupportedProbe,
		"relay":  StatusReady,
	}
	for _, record := range report.Backends {
		if record.Status != wantStatuses[record.Backend] {
			t.Fatalf("%s status = %q, want %q: %#v", record.Backend, record.Status, wantStatuses[record.Backend], record)
		}
	}
	gemini := report.Backends[2]
	if gemini.AuthenticationStatus != AuthenticationUnsupported || gemini.ProbeDetail.Authentication.Attempted || gemini.ProbeDetail.Authentication.Status != ProbeStatusUnsupported {
		t.Fatalf("gemini auth probe = %#v", gemini)
	}
	assertProbeLog(t, logPath, []string{
		"claude:--version",
		"claude:auth status --json",
		"codex:--version",
		"codex:login status",
		"gemini:--version",
	})
}

func TestClaudeAuthenticationParsesStdoutAndRetainsStderr(t *testing.T) {
	dir := t.TempDir()
	writeProbeExecutable(t, dir, "claude", `
case "$*" in
  "--version") printf 'claude 1.0\n' ;;
  "auth status --json")
    printf '{"loggedIn":true}\n'
    printf 'benign update warning\n' >&2
    ;;
  *) exit 90 ;;
esac`)
	t.Setenv("PATH", dir)

	record := checkOne(t, "claude", Options{ProbeAuth: true})
	if record.Status != StatusReady || record.AuthenticationStatus != AuthenticationAuthenticated {
		t.Fatalf("claude readiness = %#v", record)
	}
	if output := record.ProbeDetail.Authentication.Output; output != "{\"loggedIn\":true}\nbenign update warning" {
		t.Fatalf("authentication diagnostics = %q", output)
	}
}

func TestCommandCompletedSuccessfullyRejectsTimedOutResult(t *testing.T) {
	exitCode := 0
	if commandCompletedSuccessfully(commandResult{exitCode: &exitCode, timedOut: true}) {
		t.Fatal("timed-out command was accepted as successful")
	}
	if !commandCompletedSuccessfully(commandResult{exitCode: &exitCode}) {
		t.Fatal("clean zero-exit command was rejected")
	}
}

func TestReadinessNormalizesMissingFailedMalformedAndTimedOutProbes(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		records, err := Check(context.Background(), []string{"codex"}, Options{
			LookPath: func(string) (string, error) { return "", errors.New("missing test executable") },
		})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		record := records[0]
		if record.Status != StatusNotInstalled || record.ProbeDetail.Installation.Status != ProbeStatusNotInstalled || record.ProbeDetail.Version.Attempted {
			t.Fatalf("missing record = %#v", record)
		}
	})

	t.Run("version exit failure", func(t *testing.T) {
		dir := t.TempDir()
		writeProbeExecutable(t, dir, "codex", `
if [ "$1" = "--version" ]; then
  printf 'version failed\n' >&2
  exit 7
fi
exit 90`)
		t.Setenv("PATH", dir)
		record := checkOne(t, "codex", Options{})
		if record.Status != StatusProbeFailed || record.ProbeDetail.Version.ExitCode == nil || *record.ProbeDetail.Version.ExitCode != 7 {
			t.Fatalf("failed version record = %#v", record)
		}
	})

	t.Run("empty version output", func(t *testing.T) {
		dir := t.TempDir()
		writeProbeExecutable(t, dir, "codex", `
if [ "$1" = "--version" ]; then
  exit 0
fi
exit 90`)
		t.Setenv("PATH", dir)
		record := checkOne(t, "codex", Options{})
		if record.Status != StatusProbeFailed || record.ProbeDetail.Version.Status != ProbeStatusFailed || !strings.Contains(record.ProbeDetail.Version.Error, "empty output") {
			t.Fatalf("empty version record = %#v", record)
		}
	})

	t.Run("version timeout", func(t *testing.T) {
		dir := t.TempDir()
		writeProbeExecutable(t, dir, "codex", `
if [ "$1" = "--version" ]; then
  while :; do :; done
fi
exit 90`)
		t.Setenv("PATH", dir)
		record := checkOne(t, "codex", Options{Timeout: 25 * time.Millisecond})
		if record.Status != StatusProbeFailed || record.ProbeDetail.Version.Status != ProbeStatusTimedOut {
			t.Fatalf("timed-out record = %#v", record)
		}
	})

	t.Run("authentication rejected", func(t *testing.T) {
		dir := t.TempDir()
		writeProbeExecutable(t, dir, "codex", `
case "$*" in
  "--version") printf 'codex 1.0\n' ;;
  "login status") printf 'Not logged in\n' >&2; exit 1 ;;
  *) exit 90 ;;
esac`)
		t.Setenv("PATH", dir)
		record := checkOne(t, "codex", Options{ProbeAuth: true})
		if record.Status != StatusAuthFailed || record.AuthenticationStatus != AuthenticationFailed || record.ProbeDetail.Authentication.ExitCode == nil || *record.ProbeDetail.Authentication.ExitCode != 1 {
			t.Fatalf("auth-failed record = %#v", record)
		}
	})

	t.Run("malformed authentication output", func(t *testing.T) {
		dir := t.TempDir()
		writeProbeExecutable(t, dir, "claude", `
case "$*" in
  "--version") printf 'claude 1.0\n' ;;
  "auth status --json") printf 'not-json\n' ;;
  *) exit 90 ;;
esac`)
		t.Setenv("PATH", dir)
		record := checkOne(t, "claude", Options{ProbeAuth: true})
		if record.Status != StatusProbeFailed || record.AuthenticationStatus != AuthenticationProbeFailed || record.ProbeDetail.Authentication.Status != ProbeStatusFailed {
			t.Fatalf("malformed auth record = %#v", record)
		}
	})
}

func TestCheckRejectsUnknownBackendsAndReturnsCanonicalOrder(t *testing.T) {
	if _, err := Check(context.Background(), []string{"unknown"}, Options{}); err == nil {
		t.Fatal("unknown backend was accepted")
	}
	records, err := Check(context.Background(), []string{"relay", "relay"}, Options{})
	if err != nil || len(records) != 1 || records[0].Backend != "relay" {
		t.Fatalf("deduplicated records = %#v, err=%v", records, err)
	}
}

func TestFormatReportDistinguishesReadinessStates(t *testing.T) {
	report := Report{Backends: []Record{
		{Backend: "codex", Status: StatusNotInstalled, AuthenticationStatus: AuthenticationUnknown},
		{Backend: "claude", Status: StatusInstalledAuthUnknown, AuthenticationStatus: AuthenticationUnknown, ExecutablePath: "/tools/claude", Version: "claude 1"},
		{Backend: "gemini", Status: StatusUnsupportedProbe, AuthenticationStatus: AuthenticationUnsupported, ExecutablePath: "/tools/gemini", Version: "gemini 1"},
		{Backend: "relay", Status: StatusReady, AuthenticationStatus: AuthenticationNotApplicable, ExecutablePath: "built-in", Version: "built-in"},
	}}
	output := FormatReport(report)
	for _, expected := range []string{StatusNotInstalled, StatusInstalledAuthUnknown, StatusUnsupportedProbe, StatusReady, "auth=unsupported", "executable=built-in"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("formatted output missing %q:\n%s", expected, output)
		}
	}
}

func checkOne(t *testing.T, backend string, options Options) Record {
	t.Helper()
	records, err := Check(context.Background(), []string{backend}, options)
	if err != nil {
		t.Fatalf("check %s: %v", backend, err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	return records[0]
}

func writeProbeExecutable(t *testing.T, dir string, name string, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s probe: %v", name, err)
	}
	return path
}

func successfulProbeScript(name string, version string, authentication string) string {
	return `
printf '` + name + `:%s\n' "$*" >> "$READINESS_TEST_LOG"
case "$*" in
  "--version") printf '` + version + `\n' ;;
  "login status"|"auth status --json") printf '` + authentication + `\n' ;;
  *) printf 'unexpected model-capable invocation: %s\n' "$*" >&2; exit 91 ;;
esac`
}

func assertProbeLog(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probe log = %#v, want %#v", got, want)
	}
}
