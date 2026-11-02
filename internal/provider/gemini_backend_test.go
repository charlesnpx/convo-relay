package provider

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGeminiParseOutputAcceptsKnownShapesAndPlainText(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		wantText string
		wantRef  string
		wantErr  string
	}{
		{name: "response", stdout: `{"response":"Gemini reply","session_id":"gemini-session"}`, wantText: "Gemini reply", wantRef: "gemini-session"},
		{name: "text", stdout: `{"text":"Text reply","session_id":"s1"}`, wantText: "Text reply", wantRef: "s1"},
		{name: "content", stdout: `{"content":"Content reply"}`, wantText: "Content reply"},
		{name: "message", stdout: `{"message":"Message reply"}`, wantText: "Message reply"},
		{name: "structured error", stdout: `{"error":{"message":"Structured error"}}`, wantErr: "Structured error"},
		{name: "string error", stdout: `{"error":"String error"}`, wantErr: "String error"},
		{name: "response and structured error", stdout: `{"response":"Gemini reply","error":{"message":"Authentication error"}}`, wantText: "Gemini reply", wantErr: "Authentication error"},
		{name: "plain text", stdout: `plain fallback`, wantText: "plain fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ref, gotErr := parseGeminiOutput(tt.stdout)
			if text != tt.wantText || ref != tt.wantRef || gotErr != tt.wantErr {
				t.Fatalf("parseGeminiOutput = (%q, %q, %q), want (%q, %q, %q)", text, ref, gotErr, tt.wantText, tt.wantRef, tt.wantErr)
			}
		})
	}
}

func init() {
	if os.Getenv("CONVO_RELAY_TEST_GEMINI_AUTH_ERROR") == "1" {
		_, _ = os.Stdout.Write([]byte(`{"error":{"message":"Authentication error: token expired"}}`))
		os.Exit(17)
	}
}

func TestGeminiStructuredAuthErrorOnStdoutReturnsBackendRunError(t *testing.T) {
	binDir := t.TempDir()
	commandName := "gemini"
	if runtime.GOOS == "windows" {
		commandName += ".exe"
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, commandName), contents, 0o755); err != nil {
		t.Fatalf("write Gemini helper executable: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CONVO_RELAY_TEST_GEMINI_AUTH_ERROR", "1")

	backend := newGeminiBackend(t.TempDir(), "slot_0", "Gemini", t.TempDir(), SlotConfig{})
	_, err = backend.RunTurn(t.Context(), "prompt", TurnOptions{})
	var backendErr BackendRunError
	if !errors.As(err, &backendErr) {
		t.Fatalf("RunTurn error = %T %v, want BackendRunError", err, err)
	}
	if backendErr.Label != "Gemini" || backendErr.Detail != "Authentication error: token expired" {
		t.Fatalf("backend error = %#v", backendErr)
	}
}
