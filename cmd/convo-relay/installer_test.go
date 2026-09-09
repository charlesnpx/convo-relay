package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type relayInstallerResult struct {
	Schema       int                             `json:"schema"`
	Name         string                          `json:"name"`
	Version      string                          `json:"version"`
	Operation    string                          `json:"operation"`
	Kind         string                          `json:"kind"`
	Capabilities []string                        `json:"capabilities"`
	Setup        []relayInstallerSetup           `json:"setup"`
	Targets      map[string]relayInstallerTarget `json:"targets"`
	Warnings     []string                        `json:"warnings"`
}

type relayInstallerSetup struct {
	Kind        string `json:"kind"`
	Executable  string `json:"executable"`
	Remediation string `json:"remediation"`
}

type relayInstallerTarget struct {
	Files []relayInstallerFile `json:"files"`
}

type relayInstallerFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func TestInstallerPlanJSONWithoutGoOnPath(t *testing.T) {
	root := installerRepoRoot(t)
	home := t.TempDir()
	cmd := installerCommand(t, root, "--plan", "--target", "all", "--json")
	cmd.Env = installerCommandEnv(t, map[string]string{
		"HOME": home,
		"PATH": "/usr/bin:/bin",
	})
	stdout, stderr, err := runInstallerCommand(cmd)
	if err != nil {
		t.Fatalf("plan failed: %v stderr=%s stdout=%s", err, stderr, stdout)
	}

	result := decodeRelayInstallerResult(t, stdout)
	if result.Schema != 1 || result.Name != "convo-relay" || result.Operation != "plan" || result.Kind != "delegated" {
		t.Fatalf("plan result = %+v", result)
	}
	if len(result.Setup) != 1 || result.Setup[0].Kind != "executable" || result.Setup[0].Executable != "go" {
		t.Fatalf("setup = %+v", result.Setup)
	}
	if len(result.Targets) != 3 {
		t.Fatalf("targets = %+v, want tools, claude, and codex", result.Targets)
	}

	canonicalRoot := evalInstallerPath(t, home)
	wantTargets := relayInstallerPaths(canonicalRoot)
	assertRelayInstallerPaths(t, result, wantTargets, false)
}

func TestInstallerInstallAndUninstallStagedAll(t *testing.T) {
	root := installerRepoRoot(t)
	stage := t.TempDir()
	canonicalStage := evalInstallerPath(t, stage)

	installCmd := installerCommand(t, root, "--install", "--target", "all", "--json", "--install-root", stage)
	installCmd.Env = installerCommandEnv(t, map[string]string{
		"GOCACHE": installerGoCache(t),
	})
	stdout, stderr, err := runInstallerCommand(installCmd)
	if err != nil {
		t.Fatalf("install failed: %v stderr=%s stdout=%s", err, stderr, stdout)
	}

	installResult := decodeRelayInstallerResult(t, stdout)
	if installResult.Schema != 1 || installResult.Name != "convo-relay" || installResult.Operation != "install" || installResult.Kind != "delegated" {
		t.Fatalf("install result = %+v", installResult)
	}
	wantTargets := relayInstallerPaths(canonicalStage)
	assertRelayInstallerPaths(t, installResult, wantTargets, true)
	for _, target := range installResult.Targets {
		for _, file := range target.Files {
			if !installerPathWithin(canonicalStage, evalInstallerPath(t, file.Path)) {
				t.Fatalf("installed path %q escapes resolved root %q", file.Path, canonicalStage)
			}
			if got := installerFileSHA256(t, file.Path); got != file.SHA256 {
				t.Fatalf("sha256 for %s = %s, want %s", file.Path, got, file.SHA256)
			}
		}
	}

	uninstallCmd := installerCommand(t, root, "--uninstall", "--target", "all", "--json", "--install-root", stage)
	uninstallCmd.Env = installerCommandEnv(t, nil)
	uninstallOut, uninstallErr, err := runInstallerCommand(uninstallCmd)
	if err != nil {
		t.Fatalf("uninstall failed: %v stderr=%s stdout=%s", err, uninstallErr, uninstallOut)
	}
	uninstallResult := decodeRelayInstallerResult(t, uninstallOut)
	if uninstallResult.Schema != 1 || uninstallResult.Name != "convo-relay" || uninstallResult.Operation != "uninstall" || uninstallResult.Kind != "delegated" {
		t.Fatalf("uninstall result = %+v", uninstallResult)
	}
	assertRelayInstallerPaths(t, uninstallResult, wantTargets, false)
	for _, target := range installResult.Targets {
		for _, file := range target.Files {
			if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
				t.Fatalf("installed path %q remains after uninstall: %v", file.Path, err)
			}
		}
	}
}

func installerRepoRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate installer test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func installerCommand(t *testing.T, root string, args ...string) *exec.Cmd {
	t.Helper()
	script := filepath.Join(root, "install-skill.sh")
	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Dir = root
	return cmd
}

func installerCommandEnv(t *testing.T, overrides map[string]string) []string {
	t.Helper()
	env := os.Environ()
	if len(overrides) == 0 {
		return env
	}
	filtered := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		filtered = append(filtered, entry)
	}
	for key, value := range overrides {
		filtered = append(filtered, fmt.Sprintf("%s=%s", key, value))
	}
	return filtered
}

func installerGoCache(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "convo-relay-gocache-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func runInstallerCommand(cmd *exec.Cmd) (string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func decodeRelayInstallerResult(t *testing.T, raw string) relayInstallerResult {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("installer output is not JSON: %q", raw)
	}
	var result relayInstallerResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode installer JSON: %v: %s", err, raw)
	}
	return result
}

func evalInstallerPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func relayInstallerPaths(root string) map[string][]string {
	shareRoot := filepath.Join(root, ".local", "share", "convo-relay", "skill")
	return map[string][]string{
		"tools": {
			filepath.Join(root, ".local", "bin", "convo-relay"),
			filepath.Join(shareRoot, "SKILL.md"),
			filepath.Join(shareRoot, "steer", "SKILL.md"),
			filepath.Join(shareRoot, "codex", "SKILL.md"),
			filepath.Join(shareRoot, "codex", "steer", "SKILL.md"),
		},
		"claude": {
			filepath.Join(root, ".claude", "skills", "relay", "SKILL.md"),
			filepath.Join(root, ".claude", "skills", "relay:steer", "SKILL.md"),
		},
		"codex": {
			filepath.Join(root, ".codex", "skills", "relay", "SKILL.md"),
			filepath.Join(root, ".codex", "skills", "relay:steer", "SKILL.md"),
		},
	}
}

func assertRelayInstallerPaths(t *testing.T, result relayInstallerResult, want map[string][]string, installed bool) {
	t.Helper()
	if len(result.Targets) != len(want) {
		t.Fatalf("targets = %+v, want %d targets", result.Targets, len(want))
	}
	for target, wantPaths := range want {
		gotTarget, ok := result.Targets[target]
		if !ok {
			t.Fatalf("target %q missing from %+v", target, result.Targets)
		}
		if len(gotTarget.Files) != len(wantPaths) {
			t.Fatalf("target %q files = %+v, want %d files", target, gotTarget.Files, len(wantPaths))
		}
		wantSet := make(map[string]bool, len(wantPaths))
		for _, path := range wantPaths {
			wantSet[path] = true
		}
		for _, file := range gotTarget.Files {
			if !filepath.IsAbs(file.Path) {
				t.Fatalf("target %q reported non-absolute path %q", target, file.Path)
			}
			if !wantSet[file.Path] {
				t.Fatalf("target %q reported unexpected path %q", target, file.Path)
			}
			if installed {
				if file.SHA256 == "" {
					t.Fatalf("target %q installed file %q has no sha256", target, file.Path)
				}
			} else if file.SHA256 != "" {
				t.Fatalf("target %q non-install file %q has sha256 %q", target, file.Path, file.SHA256)
			}
			delete(wantSet, file.Path)
		}
		if len(wantSet) != 0 {
			t.Fatalf("target %q omitted paths: %+v", target, wantSet)
		}
	}
}

func installerPathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return path != root && strings.HasPrefix(path, root+string(filepath.Separator))
}

func installerFileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
