package gitops

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const maxAppName = 63

type Mode string

const (
	ModeCommit      Mode = "commit"
	ModePullRequest Mode = "pull_request"
)

type Format string

const (
	FormatDigest    Format = "digest"
	FormatReference Format = "reference"
)

var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

type KustomizeImage struct {
	Name string
}

type YAMLValue struct {
	File   string
	Path   []string
	Format Format
}

type Target struct {
	App             string
	Owner           string
	Repo            string
	Branch          string
	Dir             string
	ImageRepository string
	Kustomize       *KustomizeImage
	YAML            *YAMLValue
	Mode            Mode
}

type Request struct {
	Target     Target
	Digest     core.Digest
	Deployment core.DeploymentID
	Kind       core.Kind
	AppCommit  core.Commit
	RollbackOf *core.DeploymentID
	Blocks     []core.Digest
	Unblocks   []core.Digest
	Repin      bool
}

type Result struct {
	Commit      core.Commit
	Previous    core.Digest
	Changed     bool
	PullRequest int
}

type Pinned struct {
	Digest core.Digest
	Value  string
	File   string
}

func (t Target) Validate() error {
	var errs []error
	if !isDNSLabel(t.App) {
		errs = append(errs, fmt.Errorf("app %q must be a DNS label", t.App))
	}
	if t.Owner == "" || t.Repo == "" {
		errs = append(errs, errors.New("gitops repository owner and name are required"))
	}
	if err := validateBranch(t.Branch); err != nil {
		errs = append(errs, err)
	}
	if t.Dir != "" {
		if err := validateRelative(t.Dir); err != nil {
			errs = append(errs, fmt.Errorf("dir: %w", err))
		}
	}
	if err := validateImageRepository(t.ImageRepository); err != nil {
		errs = append(errs, err)
	}
	switch {
	case t.Kustomize != nil && t.YAML != nil, t.Kustomize == nil && t.YAML == nil:
		errs = append(errs, errors.New("exactly one of kustomize and yaml must be set"))
	case t.Kustomize != nil:
		if t.Kustomize.Name == "" || strings.ContainsFunc(t.Kustomize.Name, isSpaceOrControl) {
			errs = append(errs, fmt.Errorf("kustomize image name %q must be non-empty without whitespace", t.Kustomize.Name))
		}
	default:
		errs = append(errs, t.YAML.validate())
	}
	if t.Mode != ModeCommit && t.Mode != ModePullRequest {
		errs = append(errs, fmt.Errorf("unknown mode %q", t.Mode))
	}
	return errors.Join(errs...)
}

func (y YAMLValue) validate() error {
	var errs []error
	if y.File == "" {
		errs = append(errs, errors.New("yaml file is required"))
	} else if err := validateRelative(y.File); err != nil {
		errs = append(errs, fmt.Errorf("yaml file: %w", err))
	}
	if len(y.Path) == 0 {
		errs = append(errs, errors.New("yaml path is required"))
	}
	for _, segment := range y.Path {
		if segment == "" || strings.ContainsFunc(segment, unicode.IsControl) {
			errs = append(errs, fmt.Errorf("yaml path segment %q must be non-empty without control characters", segment))
		}
	}
	if y.Format != FormatDigest && y.Format != FormatReference {
		errs = append(errs, fmt.Errorf("unknown yaml format %q", y.Format))
	}
	return errors.Join(errs...)
}

func (r Request) Validate() error {
	errs := []error{r.Target.Validate()}
	if _, err := core.ParseDigest(string(r.Digest)); err != nil {
		errs = append(errs, err)
	}
	if r.Deployment <= 0 {
		errs = append(errs, fmt.Errorf("deployment id %d must be positive", r.Deployment))
	}
	if !r.Kind.Valid() {
		errs = append(errs, fmt.Errorf("unknown deployment kind %q", r.Kind))
	}
	if err := r.AppCommit.Validate(); err != nil {
		errs = append(errs, err)
	} else if err := validateRepoName(r.AppCommit.Repository); err != nil {
		errs = append(errs, fmt.Errorf("app commit: %w", err))
	}
	switch {
	case r.Kind == core.KindRollback && (r.RollbackOf == nil || *r.RollbackOf <= 0):
		errs = append(errs, errors.New("a rollback must name the deployment it rolls back"))
	case r.Kind != core.KindRollback && r.RollbackOf != nil:
		errs = append(errs, errors.New("only a rollback may name a deployment it rolls back"))
	}
	for _, block := range r.Blocks {
		if _, err := core.ParseDigest(string(block)); err != nil {
			errs = append(errs, fmt.Errorf("block: %w", err))
		}
	}
	for _, unblock := range r.Unblocks {
		if _, err := core.ParseDigest(string(unblock)); err != nil {
			errs = append(errs, fmt.Errorf("unblock: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (r Request) headBranch() string {
	return "doorstep/" + r.Target.App + "-" + strconv.FormatInt(int64(r.Deployment), 10)
}

func (t Target) candidateFiles() []string {
	if t.YAML != nil {
		return []string{path.Join(t.Dir, t.YAML.File)}
	}
	files := make([]string, 0, len(kustomizationFiles))
	for _, name := range kustomizationFiles {
		files = append(files, path.Join(t.Dir, name))
	}
	return files
}

func (t Target) desiredValue(digest core.Digest) string {
	if t.YAML != nil && t.YAML.Format == FormatReference {
		return t.ImageRepository + "@" + string(digest)
	}
	return string(digest)
}

func (t Target) digestOf(value string) core.Digest {
	if t.YAML != nil && t.YAML.Format == FormatReference {
		repository, digest, ok := strings.Cut(value, "@")
		if !ok || repository != t.ImageRepository {
			return ""
		}
		value = digest
	}
	digest, err := core.ParseDigest(value)
	if err != nil {
		return ""
	}
	return digest
}

func isDNSLabel(s string) bool {
	if s == "" || len(s) > maxAppName || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func validateBranch(branch string) error {
	if branch == "" || strings.ContainsFunc(branch, isSpaceOrControl) {
		return fmt.Errorf("branch %q must be non-empty without whitespace", branch)
	}
	return nil
}

func validateRelative(p string) error {
	if strings.HasPrefix(p, "/") || strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("%q must be a relative path", p)
	}
	for segment := range strings.SplitSeq(p, "/") {
		if segment == ".." {
			return fmt.Errorf("%q must not contain '..'", p)
		}
	}
	return nil
}

func validateImageRepository(repository string) error {
	if repository == "" || strings.ContainsFunc(repository, isSpaceOrControl) || strings.Contains(repository, "@") {
		return fmt.Errorf("image repository %q must be non-empty, without whitespace or a digest", repository)
	}
	return nil
}

func validateRepoName(name string) error {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || strings.ContainsAny(name, "@") ||
		strings.ContainsFunc(name, isSpaceOrControl) {
		return fmt.Errorf("repository %q must have the form owner/name", name)
	}
	return nil
}

func isSpaceOrControl(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}
