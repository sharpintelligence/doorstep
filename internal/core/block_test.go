package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testBlockDigest = Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	testBlockSHA    = "2222222222222222222222222222222222222222"
)

func validBlock() Block {
	return Block{
		Target:    Target{App: "blog", Environment: "production"},
		Digest:    testBlockDigest,
		Commit:    Commit{Repository: "acme/blog", SHA: testBlockSHA},
		Reason:    "broke checkout",
		CreatedBy: "user:alice",
	}
}

func TestBlockValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Block)
		wantErr bool
	}{
		{name: "digest and commit", mutate: func(*Block) {}},
		{name: "digest only", mutate: func(b *Block) { b.Commit = Commit{} }},
		{name: "commit only", mutate: func(b *Block) { b.Digest = "" }},
		{name: "commit without repository", mutate: func(b *Block) { b.Commit.Repository = "" }},
		{name: "empty reason", mutate: func(b *Block) { b.Reason = "" }},
		{name: "reason at limit", mutate: func(b *Block) { b.Reason = strings.Repeat("é", MaxBlockReasonLength) }},
		{name: "neither digest nor commit", mutate: func(b *Block) { b.Digest, b.Commit = "", Commit{} }, wantErr: true},
		{name: "repository without sha", mutate: func(b *Block) { b.Commit.SHA = "" }, wantErr: true},
		{name: "bad digest", mutate: func(b *Block) { b.Digest = "sha256:abc" }, wantErr: true},
		{name: "bad sha", mutate: func(b *Block) { b.Commit.SHA = "main" }, wantErr: true},
		{name: "repository with newline", mutate: func(b *Block) { b.Commit.Repository = "acme/blog\nx" }, wantErr: true},
		{name: "reason too long", mutate: func(b *Block) { b.Reason = strings.Repeat("a", MaxBlockReasonLength+1) }, wantErr: true},
		{name: "reason with control", mutate: func(b *Block) { b.Reason = "a\nb" }, wantErr: true},
		{name: "reason invalid utf8", mutate: func(b *Block) { b.Reason = "\xff" }, wantErr: true},
		{name: "no creator", mutate: func(b *Block) { b.CreatedBy = "  " }, wantErr: true},
		{name: "no target", mutate: func(b *Block) { b.Target = Target{} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := validBlock()
			tt.mutate(&b)
			err := b.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidBlock) {
				t.Fatalf("Validate() error = %v, want ErrInvalidBlock", err)
			}
		})
	}
}

func TestBlockActiveAndSubject(t *testing.T) {
	t.Parallel()

	cleared := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		block       Block
		wantActive  bool
		wantSubject string
		wantRef     BlockRef
	}{
		{
			name:        "digest and commit",
			block:       validBlock(),
			wantActive:  true,
			wantSubject: string(testBlockDigest) + " (commit " + testBlockSHA + ")",
			wantRef:     BlockRef{Digest: testBlockDigest, CommitSHA: testBlockSHA},
		},
		{
			name:        "digest only, cleared",
			block:       Block{Digest: testBlockDigest, ClearedAt: &cleared},
			wantSubject: string(testBlockDigest),
			wantRef:     BlockRef{Digest: testBlockDigest},
		},
		{
			name:        "commit only",
			block:       Block{Commit: Commit{SHA: testBlockSHA}},
			wantActive:  true,
			wantSubject: "commit " + testBlockSHA,
			wantRef:     BlockRef{CommitSHA: testBlockSHA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.block.Active(); got != tt.wantActive {
				t.Errorf("Active() = %v, want %v", got, tt.wantActive)
			}
			if got := tt.block.Subject(); got != tt.wantSubject {
				t.Errorf("Subject() = %q, want %q", got, tt.wantSubject)
			}
			if got := tt.block.Ref(); got != tt.wantRef {
				t.Errorf("Ref() = %+v, want %+v", got, tt.wantRef)
			}
		})
	}
}

func TestParseBlockRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    BlockRef
		wantErr bool
	}{
		{in: string(testBlockDigest), want: BlockRef{Digest: testBlockDigest}},
		{in: testBlockSHA, want: BlockRef{CommitSHA: testBlockSHA}},
		{in: "sha256:abc", wantErr: true},
		{in: "sha512:" + strings.Repeat("a", 128), wantErr: true},
		{in: "ABCDEF" + testBlockSHA[6:], wantErr: true},
		{in: testBlockSHA[:39], wantErr: true},
		{in: "", wantErr: true},
		{in: "main", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBlockRef(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBlockRef(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil {
				if !errors.Is(err, ErrInvalidBlock) {
					t.Fatalf("ParseBlockRef(%q) error = %v, want ErrInvalidBlock", tt.in, err)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("ParseBlockRef(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
			if got.String() != tt.in {
				t.Fatalf("String() = %q, want %q", got.String(), tt.in)
			}
		})
	}
}

func TestBlockRefValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     BlockRef
		wantErr bool
	}{
		{name: "digest", ref: BlockRef{Digest: testBlockDigest}},
		{name: "commit", ref: BlockRef{CommitSHA: testBlockSHA}},
		{name: "both", ref: BlockRef{Digest: testBlockDigest, CommitSHA: testBlockSHA}},
		{name: "zero", wantErr: true},
		{name: "bad digest", ref: BlockRef{Digest: "sha256:x"}, wantErr: true},
		{name: "bad commit", ref: BlockRef{CommitSHA: "x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.ref.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.ref.IsZero() != (tt.ref == BlockRef{}) {
				t.Fatalf("IsZero() = %v", tt.ref.IsZero())
			}
		})
	}
	if got := (BlockRef{Digest: testBlockDigest, CommitSHA: testBlockSHA}).String(); got != string(testBlockDigest) {
		t.Fatalf("String() = %q, want the digest", got)
	}
}

func TestBlockTrailersIsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		trailers BlockTrailers
		want     bool
	}{
		{trailers: BlockTrailers{}, want: true},
		{trailers: BlockTrailers{Blocks: []Digest{testBlockDigest}}},
		{trailers: BlockTrailers{Unblocks: []Digest{testBlockDigest}}},
	}
	for i, tt := range tests {
		if got := tt.trailers.IsZero(); got != tt.want {
			t.Errorf("case %d: IsZero() = %v, want %v", i, got, tt.want)
		}
	}
}

func TestBlockedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block Block
		want  string
	}{
		{
			name:  "with reason",
			block: validBlock(),
			want:  "blog/production " + string(testBlockDigest) + " (commit " + testBlockSHA + ") is blocked by user:alice: broke checkout",
		},
		{
			name:  "without reason",
			block: Block{Target: Target{App: "blog", Environment: "production"}, Commit: Commit{SHA: testBlockSHA}, CreatedBy: "cli:bob"},
			want:  "blog/production commit " + testBlockSHA + " is blocked by cli:bob",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("wrapped: %w", &BlockedError{Block: tt.block})
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("errors.Is(ErrBlocked) = false for %v", err)
			}
			var blocked *BlockedError
			if !errors.As(err, &blocked) || blocked.Block.CreatedBy != tt.block.CreatedBy {
				t.Fatalf("errors.As() = %v", blocked)
			}
			if got := blocked.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRollbackInProgressError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("wrapped: %w", &RollbackInProgressError{Deployment: Deployment{
		ID:     7,
		Target: Target{App: "blog", Environment: "production"},
		State:  StateSyncing,
	}})
	if !errors.Is(err, ErrRollbackInProgress) {
		t.Fatalf("errors.Is(ErrRollbackInProgress) = false")
	}
	if errors.Is(err, ErrNoRollbackTarget) {
		t.Fatalf("errors.Is(ErrNoRollbackTarget) = true")
	}
	want := "wrapped: rollback 7 of blog/production is already syncing"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}
