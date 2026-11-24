package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateGateEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		evidence string
		wantErr  string
	}{
		{name: "empty", evidence: ""},
		{name: "object", evidence: `{"job":"verify-1","attempt":2}`},
		{name: "at the size limit", evidence: `"` + strings.Repeat("x", MaxGateEvidenceBytes-2) + `"`},
		{name: "over the size limit", evidence: `"` + strings.Repeat("x", MaxGateEvidenceBytes-1) + `"`, wantErr: "gate evidence is 65537 bytes, over the 65536 byte limit"},
		{name: "invalid JSON", evidence: `{"a":`, wantErr: "gate evidence is not valid JSON"},
		{name: "NUL in value", evidence: `{"a":"x\u0000y"}`, wantErr: ErrGateEvidenceNUL.Error()},
		{name: "NUL in key", evidence: `{"\u0000":1}`, wantErr: ErrGateEvidenceNUL.Error()},
		{name: "NUL in nested array", evidence: `{"a":[1,{"b":["\u0000"]}]}`, wantErr: ErrGateEvidenceNUL.Error()},
		{name: "escaped backslash before u0000", evidence: `{"a":"\\u0000"}`},
		{name: "large number", evidence: `{"a":123456789012345678901234567890,"b":"\\u0000"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateGateEvidence(json.RawMessage(tt.evidence))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("ValidateGateEvidence(%.40s) = %v, want nil", tt.evidence, err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Errorf("ValidateGateEvidence(%.40s) = %v, want %q", tt.evidence, err, tt.wantErr)
			}
			if tt.wantErr == ErrGateEvidenceNUL.Error() && !errors.Is(err, ErrGateEvidenceNUL) {
				t.Errorf("error %v is not ErrGateEvidenceNUL", err)
			}
		})
	}
}

func TestCleanGateMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "empty", message: "", want: ""},
		{name: "short", message: "ok", want: "ok"},
		{name: "exact limit", message: strings.Repeat("a", MaxGateMessageBytes), want: strings.Repeat("a", MaxGateMessageBytes)},
		{name: "over limit", message: strings.Repeat("a", MaxGateMessageBytes+10), want: strings.Repeat("a", MaxGateMessageBytes)},
		{name: "multibyte at limit", message: strings.Repeat("a", MaxGateMessageBytes-1) + "é", want: strings.Repeat("a", MaxGateMessageBytes-1)},
		{name: "NUL removed", message: "a\x00b", want: "ab"},
		{name: "invalid UTF-8 replaced", message: "a\xffb", want: "a�b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CleanGateMessage(tt.message)
			if got != tt.want {
				t.Errorf("CleanGateMessage = %q (%d bytes), want %q (%d bytes)", got, len(got), tt.want, len(tt.want))
			}
			if !utf8.ValidString(got) || len(got) > MaxGateMessageBytes {
				t.Errorf("result is not valid UTF-8 within the limit: %d bytes", len(got))
			}
		})
	}
}
