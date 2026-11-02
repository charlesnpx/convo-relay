//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	detachedStartupTimeout = 30 * time.Second
	detachedStartupMaxSize = 4096
)

func launchDetachedRun(args []string, relayHome string) (detachedLaunchReceipt, error) {
	executable, err := os.Executable()
	if err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("locate convo-relay executable for detached run: %w", err)
	}
	return startDetachedRun(executable, args, relayHome)
}

func startDetachedRun(executable string, args []string, relayHome string) (detachedLaunchReceipt, error) {
	logDir, err := filepath.Abs(filepath.Join(v2RelayHome(relayHome), "logs"))
	if err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("resolve detached log directory: %w", err)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("create detached log directory %q: %w", logDir, err)
	}
	logFile, err := os.CreateTemp(logDir, "run-detached-*.log")
	if err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("create detached run log in %q: %w", logDir, err)
	}
	defer logFile.Close()
	if err := logFile.Chmod(0o600); err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("secure detached run log %q: %w", logFile.Name(), err)
	}
	logPath := logFile.Name()

	ackReader, ackWriter, err := os.Pipe()
	if err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("create detached startup acknowledgment pipe: %w", err)
	}
	defer ackReader.Close()
	defer ackWriter.Close()

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("open null device for detached stdin: %w", err)
	}
	defer stdin.Close()

	childArgs := append([]string{"run"}, args...)
	command := exec.Command(executable, childArgs...)
	command.Stdin = stdin
	command.Stdout = logFile
	command.Stderr = logFile
	command.ExtraFiles = []*os.File{ackWriter}
	command.Env = detachedChildEnvironment(os.Environ())
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return detachedLaunchReceipt{}, fmt.Errorf("start detached run child (log: %s): %w", logPath, err)
	}
	_ = ackWriter.Close()
	_ = logFile.Close()
	_ = stdin.Close()

	ack, err := readDetachedStartupAck(command, ackReader, logPath)
	if err != nil {
		return detachedLaunchReceipt{}, err
	}
	if strings.TrimSpace(ack.SessionID) == "" {
		stopDetachedChild(command)
		return detachedLaunchReceipt{}, fmt.Errorf("detached run returned an empty session id (log: %s)", logPath)
	}
	if err := command.Process.Release(); err != nil {
		go func() { _ = command.Wait() }()
	}
	return detachedLaunchReceipt{Kind: "launch_receipt", SessionID: ack.SessionID, LogPath: logPath}, nil
}

func detachedChildEnvironment(environment []string) []string {
	childEnvironment := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != detachedStartupAckEnv {
			childEnvironment = append(childEnvironment, entry)
		}
	}
	return append(childEnvironment, detachedStartupAckEnv+"=3")
}

func readDetachedStartupAck(command *exec.Cmd, reader *os.File, logPath string) (detachedStartupAck, error) {
	type readResult struct {
		body []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		body, err := io.ReadAll(io.LimitReader(reader, detachedStartupMaxSize+1))
		result <- readResult{body: body, err: err}
	}()

	timer := time.NewTimer(detachedStartupTimeout)
	defer timer.Stop()
	var body []byte
	select {
	case read := <-result:
		if read.err != nil {
			stopDetachedChild(command)
			return detachedStartupAck{}, fmt.Errorf("read detached startup acknowledgment (log: %s): %w", logPath, read.err)
		}
		body = read.body
	case <-timer.C:
		_ = reader.Close()
		stopDetachedChild(command)
		return detachedStartupAck{}, fmt.Errorf("timed out waiting for detached run startup acknowledgment after %s (log: %s)", detachedStartupTimeout, logPath)
	}
	_ = reader.Close()
	if len(body) > detachedStartupMaxSize {
		stopDetachedChild(command)
		return detachedStartupAck{}, fmt.Errorf("detached startup acknowledgment exceeded %d bytes (log: %s)", detachedStartupMaxSize, logPath)
	}
	var ack detachedStartupAck
	if err := json.Unmarshal(body, &ack); err != nil {
		waitErr := stopDetachedChild(command)
		return detachedStartupAck{}, detachedStartupError(logPath, waitErr, "could not decode startup acknowledgment")
	}
	if ack.SessionID == "" {
		waitErr := stopDetachedChild(command)
		return detachedStartupAck{}, detachedStartupError(logPath, waitErr, "child exited before session creation was acknowledged")
	}
	return ack, nil
}

func detachedStartupError(logPath string, waitErr error, detail string) error {
	if waitErr != nil {
		detail += ": " + waitErr.Error()
	}
	return fmt.Errorf("detached run %s; inspect log: %s", detail, logPath)
}

func stopDetachedChild(command *exec.Cmd) error {
	if command.Process != nil {
		_ = command.Process.Kill()
		return command.Wait()
	}
	return nil
}
