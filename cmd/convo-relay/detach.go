package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

const detachedStartupAckEnv = "CONVO_RELAY_INTERNAL_STARTUP_ACK_FD"

type detachedLaunchReceipt struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	LogPath   string `json:"log_path"`
}

type detachedStartupAck struct {
	SessionID string `json:"session_id"`
}

func writeDetachedLaunchReceipt(receipt detachedLaunchReceipt, jsonOutput bool) {
	if jsonOutput {
		writeJSON(receipt)
		return
	}
	fmt.Printf("Detached session %s\nLog: %s\n", receipt.SessionID, receipt.LogPath)
}

// writeDetachedStartupAck is a no-op for foreground runs. The detached
// launcher supplies one inherited pipe descriptor to its child and waits for
// this acknowledgment before printing a launch receipt.
func writeDetachedStartupAck(sessionID string) error {
	value := os.Getenv(detachedStartupAckEnv)
	if value == "" {
		return nil
	}
	_ = os.Unsetenv(detachedStartupAckEnv)
	fdes, err := strconv.Atoi(value)
	if err != nil || fdes < 3 {
		return fmt.Errorf("invalid detached startup acknowledgment descriptor %q", value)
	}
	ackFile := os.NewFile(uintptr(fdes), "detached-startup-ack")
	if ackFile == nil {
		return errors.New("detached startup acknowledgment descriptor is unavailable")
	}
	encodeErr := json.NewEncoder(ackFile).Encode(detachedStartupAck{SessionID: sessionID})
	closeErr := ackFile.Close()
	return errors.Join(encodeErr, closeErr)
}
