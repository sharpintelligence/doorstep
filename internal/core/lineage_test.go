package core

import (
	"slices"
	"testing"
)

func TestLineageValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lineage Lineage
		zero    bool
		wantErr []string
		wantIs  error
	}{
		{name: "empty", lineage: Lineage{}, zero: true},
		{name: "digest only", lineage: Lineage{Digest: validDigest}},
		{name: "everything", lineage: Lineage{
			Digest:           validDigest,
			PreviousDigest:   validDigest,
			GitOpsRepository: "acme/gitops",
			PinCommitSHA:     validSHA,
			ArgoCDRevision:   "1.2.3",
		}},
		{name: "argo cd revision is free text", lineage: Lineage{ArgoCDRevision: validSHA + ", 4.5.6"}},
		{name: "tag as digest", lineage: Lineage{Digest: "latest"}, wantIs: ErrInvalidDigest},
		{name: "bad previous digest", lineage: Lineage{PreviousDigest: "sha256:abc"}, wantErr: []string{"previous digest"}, wantIs: ErrInvalidDigest},
		{name: "short pin commit", lineage: Lineage{PinCommitSHA: "abc1234"}, wantErr: []string{"pin commit"}, wantIs: ErrInvalidCommitSHA},
		{name: "blank gitops repository", lineage: Lineage{GitOpsRepository: "  "}, wantErr: []string{"gitops repository"}},
		{name: "blank argo cd revision", lineage: Lineage{ArgoCDRevision: "\t"}, wantErr: []string{"argo cd revision"}},
		{name: "several problems", lineage: Lineage{Digest: "x", GitOpsRepository: " ", ArgoCDRevision: " "}, wantErr: []string{"gitops repository", "argo cd revision"}, wantIs: ErrInvalidDigest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.lineage.IsZero(); got != tt.zero {
				t.Errorf("IsZero() = %v, want %v", got, tt.zero)
			}
			assertError(t, tt.lineage.Validate(), tt.wantErr, tt.wantIs)
		})
	}
}

func TestTerminalStates(t *testing.T) {
	t.Parallel()

	want := []State{StateSucceeded, StateFailed, StateCancelled, StateSuperseded}
	if got := TerminalStates(); !slices.Equal(got, want) {
		t.Errorf("TerminalStates() = %v, want %v", got, want)
	}
	for _, s := range States() {
		if slices.Contains(want, s) == (s.Active() || s.Claimable()) {
			t.Errorf("state %s: terminal %v, active %v, claimable %v", s, slices.Contains(want, s), s.Active(), s.Claimable())
		}
	}
}
