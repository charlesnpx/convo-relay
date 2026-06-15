package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/charlesnpx/convo-relay/internal/store"
)

func TestQueueSteeringPromptPersistsSteeringJSON(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := store.New(sessionDir).SaveMetaMap(map[string]any{"status": "completed", "task": "Steering"}); err != nil {
		t.Fatalf("save meta: %v", err)
	}

	queued, err := QueueSteeringPrompt(sessionDir, "  Keep responses concise.  ")
	if err != nil {
		t.Fatalf("queue steering: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sessionDir, "steering.json"))
	if err != nil {
		t.Fatalf("read steering.json: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("decode steering.json: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("steering items = %#v, want one", items)
	}
	if items[0]["id"] != queued["id"] || items[0]["prompt"] != "Keep responses concise." || items[0]["source"] != "steer" {
		t.Fatalf("steering item = %#v, queued = %#v", items[0], queued)
	}
}
