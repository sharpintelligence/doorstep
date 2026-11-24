package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func parseReference(repository, tag string) (name.Tag, error) {
	if tag == "" {
		return name.Tag{}, errors.New("tag must not be empty")
	}
	if !tagPattern.MatchString(tag) {
		return name.Tag{}, fmt.Errorf("tag %q is not a valid OCI tag", tag)
	}
	if strings.Contains(repository, "://") {
		return name.Tag{}, fmt.Errorf("repository %q must not include a URL scheme", repository)
	}
	repo, err := name.NewRepository(repository)
	if err != nil {
		return name.Tag{}, err
	}
	return repo.Tag(tag), nil
}
