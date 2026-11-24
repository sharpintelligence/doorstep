package core

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxBlockReasonLength = 500

var (
	ErrBlocked      = errors.New("version is blocked")
	ErrInvalidBlock = errors.New("invalid block")
)

type BlockID int64

type Block struct {
	ID               BlockID
	Target           Target
	Digest           Digest
	Commit           Commit
	Reason           string
	CreatedBy        string
	DeploymentID     *DeploymentID
	RollbackID       *DeploymentID
	CreatedAt        time.Time
	BlockCommitSHA   string
	ClearedAt        *time.Time
	ClearedBy        string
	UnblockCommitSHA string
}

func (b Block) Active() bool {
	return b.ClearedAt == nil
}

func (b Block) Subject() string {
	switch {
	case b.Digest != "" && b.Commit.SHA != "":
		return string(b.Digest) + " (commit " + b.Commit.SHA + ")"
	case b.Digest != "":
		return string(b.Digest)
	default:
		return "commit " + b.Commit.SHA
	}
}

func (b Block) Ref() BlockRef {
	return BlockRef{Digest: b.Digest, CommitSHA: b.Commit.SHA}
}

func (b Block) Validate() error {
	errs := []error{b.Target.Validate()}
	if b.Digest == "" && b.Commit.SHA == "" {
		errs = append(errs, errors.New("a block needs a digest or a commit SHA"))
	}
	if b.Digest != "" {
		errs = append(errs, b.Digest.Validate())
	}
	if b.Commit.SHA != "" {
		errs = append(errs, ValidateCommitSHA(b.Commit.SHA))
	} else if b.Commit.Repository != "" {
		errs = append(errs, errors.New("a block commit repository needs a commit SHA"))
	}
	if strings.ContainsFunc(b.Commit.Repository, isSpaceOrControl) {
		errs = append(errs, errors.New("block commit repository must not contain whitespace or control characters"))
	}
	errs = append(errs, ValidateBlockReason(b.Reason))
	if strings.TrimSpace(b.CreatedBy) == "" {
		errs = append(errs, errors.New("block creator must not be empty"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBlock, err)
	}
	return nil
}

func ValidateBlockReason(reason string) error {
	switch n := utf8.RuneCountInString(reason); {
	case !utf8.ValidString(reason):
		return errors.New("reason must be valid UTF-8")
	case n > MaxBlockReasonLength:
		return fmt.Errorf("reason must be at most %d characters, got %d", MaxBlockReasonLength, n)
	case strings.ContainsFunc(reason, unicode.IsControl):
		return errors.New("reason must not contain control characters")
	}
	return nil
}

func isSpaceOrControl(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

type BlockRef struct {
	Digest    Digest
	CommitSHA string
}

func ParseBlockRef(s string) (BlockRef, error) {
	if strings.HasPrefix(s, digestAlgorithm) {
		d, err := ParseDigest(s)
		if err != nil {
			return BlockRef{}, fmt.Errorf("%w: %w", ErrInvalidBlock, err)
		}
		return BlockRef{Digest: d}, nil
	}
	if err := ValidateCommitSHA(s); err != nil {
		return BlockRef{}, fmt.Errorf("%w: %q must be a sha256 digest or a 40 character lowercase commit SHA", ErrInvalidBlock, s)
	}
	return BlockRef{CommitSHA: s}, nil
}

func (r BlockRef) IsZero() bool {
	return r.Digest == "" && r.CommitSHA == ""
}

func (r BlockRef) String() string {
	if r.Digest != "" {
		return string(r.Digest)
	}
	return r.CommitSHA
}

func (r BlockRef) Validate() error {
	var errs []error
	if r.IsZero() {
		errs = append(errs, errors.New("a block reference needs a digest or a commit SHA"))
	}
	if r.Digest != "" {
		errs = append(errs, r.Digest.Validate())
	}
	if r.CommitSHA != "" {
		errs = append(errs, ValidateCommitSHA(r.CommitSHA))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBlock, err)
	}
	return nil
}

type BlockTrailers struct {
	Blocks   []Digest
	Unblocks []Digest
}

func (t BlockTrailers) IsZero() bool {
	return len(t.Blocks) == 0 && len(t.Unblocks) == 0
}

type BlockedError struct {
	Block Block
}

func (e *BlockedError) Error() string {
	msg := e.Block.Target.String() + " " + e.Block.Subject() + " is blocked by " + e.Block.CreatedBy
	if e.Block.Reason != "" {
		msg += ": " + e.Block.Reason
	}
	return msg
}

func (e *BlockedError) Is(target error) bool {
	return target == ErrBlocked
}
