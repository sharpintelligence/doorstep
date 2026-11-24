package github

import (
	"errors"
	"testing"
	"time"
)

func TestInstallationID(t *testing.T) {
	t.Parallel()

	type lookup struct {
		owner, repo string
		advance     time.Duration
		wantID      int64
		wantErr     error
	}
	tests := []struct {
		name            string
		setup           func(f *fixture)
		lookups         []lookup
		wantLookupCalls int64
	}{
		{
			name:            "found",
			setup:           func(f *fixture) { f.srv.Install("acme", "app", 11) },
			lookups:         []lookup{{owner: "acme", repo: "app", wantID: 11}},
			wantLookupCalls: 1,
		},
		{
			name:  "cached",
			setup: func(f *fixture) { f.srv.Install("acme", "app", 11) },
			lookups: []lookup{
				{owner: "acme", repo: "app", wantID: 11},
				{owner: "acme", repo: "app", advance: 59 * time.Minute, wantID: 11},
			},
			wantLookupCalls: 1,
		},
		{
			name:  "cache key ignores case",
			setup: func(f *fixture) { f.srv.Install("Acme", "App", 11) },
			lookups: []lookup{
				{owner: "acme", repo: "app", wantID: 11},
				{owner: "ACME", repo: "APP", wantID: 11},
			},
			wantLookupCalls: 1,
		},
		{
			name:  "refetched after an hour",
			setup: func(f *fixture) { f.srv.Install("acme", "app", 11) },
			lookups: []lookup{
				{owner: "acme", repo: "app", wantID: 11},
				{owner: "acme", repo: "app", advance: time.Hour, wantID: 11},
			},
			wantLookupCalls: 2,
		},
		{
			name: "several owners resolve to their own installations",
			setup: func(f *fixture) {
				f.srv.Install("acme", "app", 11)
				f.srv.Install("acme", "gitops", 11)
				f.srv.Install("alice", "dotfiles", 22)
			},
			lookups: []lookup{
				{owner: "acme", repo: "app", wantID: 11},
				{owner: "alice", repo: "dotfiles", wantID: 22},
				{owner: "acme", repo: "gitops", wantID: 11},
			},
			wantLookupCalls: 3,
		},
		{
			name:  "not installed is not cached",
			setup: func(*fixture) {},
			lookups: []lookup{
				{owner: "acme", repo: "app", wantErr: ErrNotInstalled},
				{owner: "acme", repo: "app", wantErr: ErrNotInstalled},
			},
			wantLookupCalls: 2,
		},
		{
			name:  "repository outside the selected set",
			setup: func(f *fixture) { f.srv.Install("acme", "app", 11) },
			lookups: []lookup{
				{owner: "acme", repo: "secret", wantErr: ErrNotInstalled},
			},
			wantLookupCalls: 1,
		},
		{
			name: "suspended is not cached",
			setup: func(f *fixture) {
				f.srv.Install("acme", "app", 11)
				f.srv.Suspend(11)
			},
			lookups: []lookup{
				{owner: "acme", repo: "app", wantErr: ErrInstallationSuspended},
				{owner: "acme", repo: "app", wantErr: ErrInstallationSuspended},
			},
			wantLookupCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tt.setup(f)
			for i, l := range tt.lookups {
				f.clock.Advance(l.advance)
				got, err := f.app.installationID(t.Context(), l.owner, l.repo)
				if !errors.Is(err, l.wantErr) || (l.wantErr == nil && err != nil) {
					t.Fatalf("lookup %d: error = %v, want %v", i, err, l.wantErr)
				}
				if got != l.wantID {
					t.Errorf("lookup %d: id = %d, want %d", i, got, l.wantID)
				}
			}
			if got := f.srv.LookupCalls(); got != tt.wantLookupCalls {
				t.Errorf("lookup calls = %d, want %d", got, tt.wantLookupCalls)
			}
		})
	}
}

func TestRepoClientRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	tests := []struct {
		name        string
		owner, repo string
	}{
		{name: "empty owner", owner: "", repo: "app"},
		{name: "empty repo", owner: "acme", repo: ""},
		{name: "dot dot repo", owner: "acme", repo: ".."},
		{name: "dot repo", owner: "acme", repo: "."},
		{name: "slash in repo", owner: "acme", repo: "a/b"},
		{name: "slash in owner", owner: "acme/app", repo: "x"},
		{name: "dot dot owner", owner: "..", repo: "app"},
		{name: "leading hyphen", owner: "-acme", repo: "app"},
		{name: "trailing hyphen", owner: "acme-", repo: "app"},
		{name: "space in owner", owner: "ac me", repo: "app"},
		{name: "space in repo", owner: "acme", repo: "my app"},
		{name: "encoded slash", owner: "acme", repo: "a%2Fb"},
		{name: "query in repo", owner: "acme", repo: "app?x=1"},
		{name: "underscore in owner", owner: "ac_me", repo: "app"},
		{name: "unicode", owner: "acmé", repo: "app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.app.RepoClient(t.Context(), tt.owner, tt.repo); !errors.Is(err, ErrInvalidRepository) {
				t.Errorf("RepoClient(%q, %q) error = %v, want ErrInvalidRepository", tt.owner, tt.repo, err)
			}
		})
	}
	t.Cleanup(func() {
		if got := f.srv.LookupCalls(); got != 0 {
			t.Errorf("lookup calls = %d, want 0", got)
		}
	})
}

func TestValidateRepositoryAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct{ owner, repo string }{
		{owner: "a", repo: "b"},
		{owner: "acme-corp", repo: "app"},
		{owner: "Acme1", repo: "my_app.v2-final"},
		{owner: "alice", repo: ".github"},
		{owner: "x", repo: "..."},
	}
	for _, tt := range tests {
		t.Run(tt.owner+"/"+tt.repo, func(t *testing.T) {
			t.Parallel()
			if err := validateRepository(tt.owner, tt.repo); err != nil {
				t.Errorf("validateRepository: %v", err)
			}
		})
	}
}
