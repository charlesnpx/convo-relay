package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charlesnpx/convo-relay/internal/contracts"
	"github.com/charlesnpx/convo-relay/internal/gitexec"
	"github.com/charlesnpx/convo-relay/internal/model"
	"github.com/charlesnpx/convo-relay/internal/store"
)

func defaultRelayHome() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_CLAUDE_HOME")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-claude"
	}
	return filepath.Join(home, ".codex-claude")
}

func generateSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(raw[0:4]),
		hex.EncodeToString(raw[4:6]),
		hex.EncodeToString(raw[6:8]),
		hex.EncodeToString(raw[8:10]),
		hex.EncodeToString(raw[10:16]),
	)
}

func sessionDirForID(home string, sessionID string) string {
	if home == "" {
		home = defaultRelayHome()
	}
	return filepath.Join(home, "sessions", sessionID)
}

func relayHomeForSessionDir(sessionDir string) string {
	cleaned := filepath.Clean(sessionDir)
	if filepath.Base(filepath.Dir(cleaned)) == "sessions" {
		return filepath.Dir(filepath.Dir(cleaned))
	}
	return defaultRelayHome()
}

func SessionDirForID(home string, sessionID string) string {
	return sessionDirForID(home, sessionID)
}

func sessionIDFromDir(sessionDir string) string {
	return filepath.Base(filepath.Clean(sessionDir))
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func loadSessionMeta(sessionDir string) (model.SessionMeta, error) {
	return store.New(sessionDir).LoadMeta()
}

func loadMeta(sessionDir string) (map[string]any, error) {
	meta, err := loadSessionMeta(sessionDir)
	if err != nil {
		return nil, err
	}
	return meta.ToMap(), nil
}

func loadSessionTranscript(sessionDir string) (model.Transcript, error) {
	return store.New(sessionDir).LoadTranscript()
}

func loadTranscript(sessionDir string) ([]map[string]any, error) {
	transcript, err := loadSessionTranscript(sessionDir)
	if err != nil {
		return nil, err
	}
	return transcript.ToMaps(), nil
}

func saveSessionTranscript(st *store.Store, transcript model.Transcript) error {
	return st.SaveTranscript(transcript)
}

func saveTranscript(st *store.Store, transcript []map[string]any) error {
	items := make([]any, 0, len(transcript))
	for _, entry := range transcript {
		items = append(items, entry)
	}
	return st.SaveTranscriptItems(items)
}

func loadJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value, err := contracts.DecodeJSONObjectBytes(data)
	if err != nil {
		return nil, err
	}
	return value, nil
}

func writePID(sessionDir string) error {
	return os.WriteFile(filepath.Join(sessionDir, "relay.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func removePID(sessionDir string) {
	_ = os.Remove(filepath.Join(sessionDir, "relay.pid"))
}

func readPID(sessionDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, "relay.pid"))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalProcess(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

func ensureSessionDir(sessionDir string) error {
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return err
	}
	return ensureGitRepo(sessionDir)
}

func ensureGitRepo(path string) error {
	if pathHasGitRepo(path) {
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	if _, err := gitexec.Run(context.Background(), "git", path, nil, "init"); err != nil {
		return nil
	}
	_, _ = gitexec.Run(
		context.Background(),
		"git",
		path,
		map[string]string{
			"GIT_AUTHOR_NAME":     "Relay",
			"GIT_AUTHOR_EMAIL":    "relay@example.invalid",
			"GIT_COMMITTER_NAME":  "Relay",
			"GIT_COMMITTER_EMAIL": "relay@example.invalid",
		},
		"commit", "--allow-empty", "-m", "relay session init",
	)
	return nil
}

func pathHasGitRepo(path string) bool {
	root, ok := gitRootForPath(path)
	if !ok {
		return false
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	canonicalPath, err = filepath.Abs(canonicalPath)
	if err != nil {
		return false
	}
	return filepath.Clean(canonicalPath) == root
}

func pathWithinGitRepo(path string) bool {
	_, ok := gitRootForPath(path)
	return ok
}

func gitRootForPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	output, err := gitexec.Run(context.Background(), "git", path, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(output))
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", false
	}
	return filepath.Clean(root), true
}
