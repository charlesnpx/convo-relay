package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CommandError preserves a content-bounded Git diagnostic without exposing
// the ambient environment used to launch the command.
type CommandError struct {
	Args   []string
	Detail string
	Cause  error
}

type OutputRecordLimitError struct {
	Limit    int64
	Observed int64
}

func (e *OutputRecordLimitError) Error() string {
	if e == nil {
		return "Git output record limit exceeded"
	}
	return fmt.Sprintf("Git output records exceed limit: observed %d, limit %d", e.Observed, e.Limit)
}

func (e *CommandError) Error() string {
	if e == nil {
		return "git command failed"
	}
	if strings.TrimSpace(e.Detail) != "" {
		return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), strings.TrimSpace(e.Detail))
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Cause)
}

func (e *CommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// PreparedCommand owns the temporary, verified-empty Git profile used by one
// command. Close must be called after the process exits.
type PreparedCommand struct {
	Command *exec.Cmd
	root    string
}

func (p *PreparedCommand) Close() error {
	if p == nil || strings.TrimSpace(p.root) == "" {
		return nil
	}
	root := p.root
	p.root = ""
	return os.RemoveAll(root)
}

// Prepare constructs an orchestration-owned Git command with no inherited
// GIT_* overrides. Repository-local configuration remains readable, but the
// settings that can execute hooks, filters, fsmonitor helpers, maintenance, or
// replacement objects are overridden by the verified temporary profile.
func Prepare(
	ctx context.Context,
	binary string,
	cwd string,
	extraEnvironment map[string]string,
	args ...string,
) (*PreparedCommand, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	profileRoot, err := os.MkdirTemp("", "convo-relay-git-profile-")
	if err != nil {
		return nil, fmt.Errorf("create sanitized Git profile: %w", err)
	}
	fail := func(cause error) (*PreparedCommand, error) {
		return nil, errors.Join(cause, os.RemoveAll(profileRoot))
	}
	hooksDir := filepath.Join(profileRoot, "hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		return fail(fmt.Errorf("create empty Git hooks directory: %w", err))
	}
	attributesPath := filepath.Join(profileRoot, "attributes")
	if err := os.WriteFile(attributesPath, nil, 0o600); err != nil {
		return fail(fmt.Errorf("create empty global Git attributes file: %w", err))
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil || len(entries) != 0 {
		if err == nil {
			err = errors.New("hooks directory is not empty")
		}
		return fail(fmt.Errorf("verify empty Git hooks directory: %w", err))
	}

	commandArgs := make([]string, 0, len(args)+24)
	if strings.TrimSpace(cwd) != "" {
		commandArgs = append(commandArgs, "-C", cwd)
	}
	commandArgs = append(commandArgs,
		"-c", "core.hooksPath="+hooksDir,
		"-c", "core.attributesFile="+attributesPath,
		"-c", "core.fsmonitor=false",
		"-c", "core.sparseCheckout=false",
		"-c", "core.sparseCheckoutCone=false",
		"-c", "maintenance.auto=false",
		"-c", "gc.auto=0",
		"-c", "protocol.file.allow=never",
	)
	commandArgs = append(commandArgs, args...)

	command := exec.CommandContext(ctx, binary, commandArgs...)
	command.Env = controlledEnvironment(os.Environ(), extraEnvironment)
	return &PreparedCommand{Command: command, root: profileRoot}, nil
}

// Run executes one sanitized Git command and captures combined output.
func Run(
	ctx context.Context,
	binary string,
	cwd string,
	extraEnvironment map[string]string,
	args ...string,
) ([]byte, error) {
	prepared, err := Prepare(ctx, binary, cwd, extraEnvironment, args...)
	if err != nil {
		return nil, err
	}
	output, runErr := prepared.Command.CombinedOutput()
	closeErr := prepared.Close()
	if runErr != nil {
		runErr = &CommandError{
			Args:   append([]string(nil), args...),
			Detail: boundedDetail(output),
			Cause:  runErr,
		}
	}
	return output, errors.Join(runErr, closeErr)
}

// RunNULRecords executes a sanitized Git command while applying a record
// ceiling as stdout is read. It prevents ls-files/ls-tree style commands from
// buffering output beyond the caller's repository entry ceiling.
func RunNULRecords(
	ctx context.Context,
	binary string,
	cwd string,
	extraEnvironment map[string]string,
	maxRecords int64,
	args ...string,
) ([]byte, error) {
	if maxRecords <= 0 {
		return Run(ctx, binary, cwd, extraEnvironment, args...)
	}
	prepared, err := Prepare(ctx, binary, cwd, extraEnvironment, args...)
	if err != nil {
		return nil, err
	}
	stdout, err := prepared.Command.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	var stderr boundedOutputBuffer
	prepared.Command.Stderr = &stderr
	if err := prepared.Command.Start(); err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	var output bytes.Buffer
	buffer := make([]byte, 32*1024)
	var records int64
	var limitErr error
	for {
		count, readErr := stdout.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			records += int64(bytes.Count(chunk, []byte{0}))
			if records > maxRecords {
				limitErr = &OutputRecordLimitError{Limit: maxRecords, Observed: records}
				if prepared.Command.Process != nil {
					_ = prepared.Command.Process.Kill()
				}
				break
			}
			_, _ = output.Write(chunk)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				limitErr = readErr
			}
			break
		}
	}
	runErr := prepared.Command.Wait()
	closeErr := prepared.Close()
	if limitErr != nil {
		return nil, errors.Join(limitErr, closeErr)
	}
	if runErr != nil {
		runErr = &CommandError{
			Args:   append([]string(nil), args...),
			Detail: stderr.String(),
			Cause:  runErr,
		}
	}
	return output.Bytes(), errors.Join(runErr, closeErr)
}

// RunWithInput executes one sanitized Git command with orchestration-owned
// standard input and captures combined output.
func RunWithInput(
	ctx context.Context,
	binary string,
	cwd string,
	extraEnvironment map[string]string,
	input []byte,
	args ...string,
) ([]byte, error) {
	prepared, err := Prepare(ctx, binary, cwd, extraEnvironment, args...)
	if err != nil {
		return nil, err
	}
	prepared.Command.Stdin = bytes.NewReader(input)
	output, runErr := prepared.Command.CombinedOutput()
	closeErr := prepared.Close()
	if runErr != nil {
		runErr = &CommandError{
			Args:   append([]string(nil), args...),
			Detail: boundedDetail(output),
			Cause:  runErr,
		}
	}
	return output, errors.Join(runErr, closeErr)
}

func controlledEnvironment(environ []string, extra map[string]string) []string {
	result := make([]string, 0, len(environ)+10+len(extra))
	for _, entry := range environ {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		upper := strings.ToUpper(key)
		if upper == "LC_ALL" || strings.HasPrefix(upper, "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	result = append(result,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\x00") {
			continue
		}
		result = append(result, key+"="+extra[key])
	}
	return result
}

func boundedDetail(output []byte) string {
	const maximum = 16 * 1024
	if len(output) > maximum {
		output = output[len(output)-maximum:]
	}
	return strings.TrimSpace(string(output))
}

type boundedOutputBuffer struct {
	data []byte
}

func (b *boundedOutputBuffer) Write(data []byte) (int, error) {
	const maximum = 16 * 1024
	b.data = append(b.data, data...)
	if len(b.data) > maximum {
		b.data = append([]byte(nil), b.data[len(b.data)-maximum:]...)
	}
	return len(data), nil
}

func (b *boundedOutputBuffer) String() string {
	return strings.TrimSpace(string(b.data))
}
