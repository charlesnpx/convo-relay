package runner

import "github.com/charlesnpx/convo-relay/internal/provider"

func newGeminiBackend(sessionRoot string, slotID string, label string, cwd string, config SlotConfig) Backend {
	backend, err := provider.NewBackend("gemini", sessionRoot, slotID, label, cwd, config)
	if err != nil {
		panic(err)
	}
	return backend
}

func parseGeminiOutput(stdout string) (string, string) {
	return provider.ParseGeminiOutput(stdout)
}
