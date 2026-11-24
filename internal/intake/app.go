package intake

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type App struct {
	Target          core.Target
	Repository      string
	Branch          string
	RequiredChecks  []string
	ImageRepository string
}

func (a App) Validate() error {
	errs := []error{a.Target.Validate()}
	if _, _, err := splitRepository(a.Repository); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(a.Branch) == "" {
		errs = append(errs, errors.New("branch must not be empty"))
	}
	if strings.TrimSpace(a.ImageRepository) == "" {
		errs = append(errs, errors.New("image repository must not be empty"))
	}
	for i, name := range a.RequiredChecks {
		if strings.TrimSpace(name) == "" {
			errs = append(errs, fmt.Errorf("required check %d must not be empty", i))
		}
	}
	return errors.Join(errs...)
}

func (a App) matches(repository, branch string) bool {
	return strings.EqualFold(a.Repository, repository) && a.Branch == branch
}

func (a App) deployment(c core.TrackedCommit) core.Deployment {
	return core.Deployment{
		Target:      a.Target,
		Kind:        core.KindDeploy,
		Source:      c.Source,
		RequestedBy: c.PushedBy,
		Commit:      c.Commit,
		Image:       core.ImageRef{Repository: a.ImageRepository, Tag: c.Commit.SHA},
	}
}

func splitRepository(repository string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository %q must be owner/name", repository)
	}
	return owner, name, nil
}
