package provider

import (
	"context"
	"testing"
	"time"
)

func TestClassifyRetryableProviderErrorPolicy(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		retryable bool
	}{
		{name: "api error", text: "API Error: rate limit exceeded", retryable: true},
		{name: "http status", text: "request failed with status code 503", retryable: true},
		{name: "auth", text: "Authentication error: token expired", retryable: false},
		{name: "network", text: "network error: connection reset", retryable: true},
		{name: "missing binary", text: "command not found: claude", retryable: false},
		{name: "session collision", text: "Session ID abc is already in use", retryable: false},
		{name: "invalid option", text: "unknown option --bad", retryable: false},
		{name: "empty", text: "   ", retryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyRetryableProviderError(tt.text) != ""
			if got != tt.retryable {
				t.Fatalf("retryable = %v, want %v for %q", got, tt.retryable, tt.text)
			}
		})
	}
}

func withFakeRetryBackoff(t *testing.T, fake func(context.Context, time.Duration) error) {
	t.Helper()
	original := retryBackoff
	retryBackoff = fake
	t.Cleanup(func() {
		retryBackoff = original
	})
}
