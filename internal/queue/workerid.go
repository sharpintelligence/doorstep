package queue

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const unknownHost = "doorstep"

func NewWorkerID() (string, error) {
	return workerID(os.Hostname)
}

func workerID(hostname func() (string, error)) (string, error) {
	host, err := hostname()
	host = strings.TrimSpace(host)
	if err != nil || host == "" {
		host = unknownHost
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("worker ID: %w", err)
	}
	return host + "-" + hex.EncodeToString(suffix), nil
}
