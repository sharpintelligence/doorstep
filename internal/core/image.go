package core

import (
	"errors"
	"fmt"
	"strings"
)

const (
	digestAlgorithm = "sha256:"
	digestHexLength = 64
)

var ErrInvalidDigest = errors.New("invalid image digest")

type Digest string

func ParseDigest(s string) (Digest, error) {
	hex, ok := strings.CutPrefix(s, digestAlgorithm)
	if !ok || !isLowerHex(hex, digestHexLength) {
		return "", fmt.Errorf("%w: %q must be sha256: followed by %d lowercase hex characters", ErrInvalidDigest, s, digestHexLength)
	}
	return Digest(s), nil
}

func (d Digest) String() string {
	return string(d)
}

func (d Digest) Validate() error {
	_, err := ParseDigest(string(d))
	return err
}

type ImageRef struct {
	Repository string
	Tag        string
}

func (r ImageRef) String() string {
	return r.Repository + ":" + r.Tag
}

func (r ImageRef) Validate() error {
	var errs []error
	if strings.TrimSpace(r.Repository) == "" {
		errs = append(errs, errors.New("image repository must not be empty"))
	}
	if strings.TrimSpace(r.Tag) == "" {
		errs = append(errs, errors.New("image tag must not be empty"))
	}
	return errors.Join(errs...)
}

func isLowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
