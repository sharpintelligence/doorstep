package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxGateMessageBytes  = 1024
	MaxGateEvidenceBytes = 64 << 10
)

var ErrGateEvidenceNUL = errors.New(`gate evidence contains \u0000, which Postgres cannot store`)

func CleanGateMessage(message string) string {
	message = strings.ToValidUTF8(strings.ReplaceAll(message, "\x00", ""), "�")
	if len(message) <= MaxGateMessageBytes {
		return message
	}
	cut := MaxGateMessageBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut]
}

func ValidateGateEvidence(evidence json.RawMessage) error {
	if len(evidence) == 0 {
		return nil
	}
	if len(evidence) > MaxGateEvidenceBytes {
		return fmt.Errorf("gate evidence is %d bytes, over the %d byte limit", len(evidence), MaxGateEvidenceBytes)
	}
	if !json.Valid(evidence) {
		return errors.New("gate evidence is not valid JSON")
	}
	if !bytes.Contains(evidence, []byte(`\u0000`)) {
		return nil
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(evidence))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode gate evidence: %w", err)
	}
	if containsNUL(decoded) {
		return ErrGateEvidenceNUL
	}
	return nil
}

func containsNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		for _, item := range v {
			if containsNUL(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range v {
			if strings.ContainsRune(key, 0) || containsNUL(item) {
				return true
			}
		}
	}
	return false
}
