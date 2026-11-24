package gitops

import (
	"errors"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestRepinWritesOverwrittenPinAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		request   func() Request
		wantPulls int
	}{
		{name: "commit mode", request: deployRequest},
		{name: "pull request mode", request: pullRequestRequest, wantPulls: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", kustomizationPath, kustomization(digestA))
			first, err := f.writer.Pin(t.Context(), tt.request())
			if err != nil {
				t.Fatalf("first Pin() error = %v", err)
			}
			seen := map[string]bool{first.Commit.SHA: true}
			for round, other := range []core.DeploymentID{43, 44} {
				newer := tt.request()
				newer.Deployment = other
				newer.Digest = digestD
				if _, err := f.writer.Pin(t.Context(), newer); err != nil {
					t.Fatalf("round %d: newer Pin() error = %v", round, err)
				}
				if _, err := f.writer.Pin(t.Context(), tt.request()); !errors.Is(err, ErrPinOverwritten) {
					t.Fatalf("round %d: replayed Pin() error = %v, want ErrPinOverwritten", round, err)
				}

				repin := tt.request()
				repin.Repin = true
				got, err := f.writer.Pin(t.Context(), repin)
				if err != nil {
					t.Fatalf("round %d: Repin error = %v", round, err)
				}
				if !got.Changed || got.Previous != digestD || got.Commit.SHA == "" || seen[got.Commit.SHA] {
					t.Fatalf("round %d: Repin result = %+v, want a new commit replacing %s", round, got, digestD)
				}
				seen[got.Commit.SHA] = true
				if got.Commit.SHA != f.mainSHA(t) {
					t.Fatalf("round %d: Repin commit %s is not the branch head %s", round, got.Commit.SHA, f.mainSHA(t))
				}
				if content := f.mainFile(t, kustomizationPath); content != kustomization(digestB) {
					t.Fatalf("round %d: file =\n%s", round, content)
				}
				if message := f.mainMessage(t); !strings.Contains(message, "Doorstep-Deployment: 42") {
					t.Fatalf("round %d: message = %q, want the rollback's own trailers", round, message)
				}
			}
			if n := len(f.repo.Pulls()); n != tt.wantPulls {
				t.Fatalf("pull requests = %d, want %d", n, tt.wantPulls)
			}
		})
	}
}

func TestRepinOfCurrentPinIsANoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request func() Request
	}{
		{name: "commit mode", request: deployRequest},
		{name: "pull request mode", request: pullRequestRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, Options{})
			f.repo.Seed("main", kustomizationPath, kustomization(digestA))
			first, err := f.writer.Pin(t.Context(), tt.request())
			if err != nil {
				t.Fatalf("first Pin() error = %v", err)
			}
			head := f.mainSHA(t)
			repin := tt.request()
			repin.Repin = true
			got, err := f.writer.Pin(t.Context(), repin)
			if err != nil || got.Changed || got.Commit.SHA != first.Commit.SHA || f.mainSHA(t) != head {
				t.Fatalf("Repin of the current pin = %+v, %v, want the first commit and no write", got, err)
			}
		})
	}
}
