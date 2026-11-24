package intake

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	MaxMigrationRange  = 50
	maxMigrationLabels = 10
	maxReasonsListed   = 3
	shortSHALength     = 7
)

var trailerPattern = regexp.MustCompile(`(?im)^[\s*>-]*doorstep-migration[ \t]*:[ \t]*(true|yes)[ \t.\r]*$`)

type MigrationPolicy struct {
	Labels  []string
	Trailer bool
}

func (p MigrationPolicy) validate() error {
	var errs []error
	if len(p.Labels) == 0 && !p.Trailer {
		errs = append(errs, errors.New("migration policy needs at least one label or the trailer"))
	}
	if len(p.Labels) > maxMigrationLabels {
		errs = append(errs, fmt.Errorf("migration policy has %d labels, more than %d", len(p.Labels), maxMigrationLabels))
	}
	for i, label := range p.Labels {
		if strings.TrimSpace(label) == "" {
			errs = append(errs, fmt.Errorf("migration label %d must not be empty", i))
		}
	}
	return errors.Join(errs...)
}

func (p MigrationPolicy) clone() *MigrationPolicy {
	p.Labels = append([]string(nil), p.Labels...)
	return &p
}

func (p MigrationPolicy) label(labels []string) (string, bool) {
	for _, have := range labels {
		for _, want := range p.Labels {
			if strings.EqualFold(have, want) {
				return have, true
			}
		}
	}
	return "", false
}

type MigrationFlag struct {
	Flagged bool
	Reason  string
}

func HasMigrationTrailer(message string) bool {
	return trailerPattern.MatchString(message)
}

func detectMigration(policy MigrationPolicy, branch string, commits []github.CommitInfo, pulls map[string][]github.CommitPullRequest) MigrationFlag {
	var reasons []string
	for _, c := range commits {
		for _, pr := range pulls[c.SHA] {
			if !pr.Merged || pr.BaseRef != branch {
				continue
			}
			if label, ok := policy.label(pr.Labels); ok {
				reasons = append(reasons, "label "+label+" on #"+strconv.Itoa(pr.Number)+" ("+shortSHA(c.SHA)+")")
			}
		}
		if policy.Trailer && HasMigrationTrailer(c.Message) {
			reasons = append(reasons, "trailer Doorstep-Migration in "+shortSHA(c.SHA))
		}
	}
	if len(reasons) == 0 {
		return MigrationFlag{}
	}
	reason := strings.Join(reasons[:min(len(reasons), maxReasonsListed)], "; ")
	if extra := len(reasons) - maxReasonsListed; extra > 0 {
		reason += "; +" + strconv.Itoa(extra) + " more"
	}
	return MigrationFlag{Flagged: true, Reason: core.CleanMigrationReason(reason)}
}

func tooManyCommits(total int) MigrationFlag {
	return MigrationFlag{
		Flagged: true,
		Reason:  core.CleanMigrationReason(strconv.Itoa(total) + " commits since the last release, too many to inspect"),
	}
}

func shortSHA(sha string) string {
	return sha[:min(len(sha), shortSHALength)]
}

type rangeKey struct {
	repository string
	base       string
	head       string
}

type commitKey struct {
	repository string
	sha        string
}

type rangeRead struct {
	commits []github.CommitInfo
	flag    *MigrationFlag
	err     error
}

type pullsRead struct {
	pulls []github.CommitPullRequest
	err   error
}

type migrationReader struct {
	source  MigrationSource
	ranges  map[rangeKey]rangeRead
	pulls   map[commitKey]pullsRead
	commits map[commitKey]rangeRead
}

func newMigrationReader(source MigrationSource) *migrationReader {
	return &migrationReader{
		source:  source,
		ranges:  map[rangeKey]rangeRead{},
		pulls:   map[commitKey]pullsRead{},
		commits: map[commitKey]rangeRead{},
	}
}

func (s *Service) migration(ctx context.Context, reader *migrationReader, app App, commit core.Commit) (MigrationFlag, error) {
	baseline, found, err := s.baselines.LastDeployCommit(ctx, app.Target)
	if err != nil {
		return MigrationFlag{}, fmt.Errorf("read the last enqueued deploy of %s: %w", app.Target, err)
	}
	var base string
	if found && baseline.SHA != commit.SHA && strings.EqualFold(baseline.Repository, commit.Repository) {
		base = baseline.SHA
	}
	read := reader.commitsSince(ctx, commit.Repository, base, commit.SHA)
	if read.err != nil {
		return MigrationFlag{}, read.err
	}
	if read.flag != nil {
		return *read.flag, nil
	}
	pulls := map[string][]github.CommitPullRequest{}
	if len(app.Migrations.Labels) > 0 {
		for _, c := range read.commits {
			p := reader.pullRequests(ctx, commit.Repository, c.SHA)
			if p.err != nil {
				return MigrationFlag{}, p.err
			}
			pulls[c.SHA] = p.pulls
		}
	}
	return detectMigration(*app.Migrations, app.Branch, read.commits, pulls), nil
}

func (r *migrationReader) commitsSince(ctx context.Context, repository, base, head string) rangeRead {
	if base == "" {
		return r.headOnly(ctx, repository, head)
	}
	key := rangeKey{strings.ToLower(repository), base, head}
	if read, ok := r.ranges[key]; ok {
		return read
	}
	read := r.readRange(ctx, repository, base, head)
	r.ranges[key] = read
	return read
}

func (r *migrationReader) readRange(ctx context.Context, repository, base, head string) rangeRead {
	commits, err := r.source.Range(ctx, repository, base, head, MaxMigrationRange)
	switch {
	case errors.Is(err, github.ErrCommitNotFound):
		return r.headOnly(ctx, repository, head)
	case err != nil:
		return rangeRead{err: fmt.Errorf("compare %s...%s in %s: %w", base, head, repository, err)}
	}
	switch commits.Status {
	case github.ComparisonBehind, github.ComparisonIdentical:
		return r.headOnly(ctx, repository, head)
	case github.ComparisonAhead, github.ComparisonDiverged:
	}
	if commits.Total > MaxMigrationRange {
		flag := tooManyCommits(commits.Total)
		return rangeRead{flag: &flag}
	}
	if len(commits.Commits) == 0 {
		return r.headOnly(ctx, repository, head)
	}
	return rangeRead{commits: commits.Commits}
}

func (r *migrationReader) headOnly(ctx context.Context, repository, head string) rangeRead {
	key := commitKey{strings.ToLower(repository), head}
	if read, ok := r.commits[key]; ok {
		return read
	}
	c, err := r.source.Commit(ctx, repository, head)
	read := rangeRead{commits: []github.CommitInfo{c}}
	if err != nil {
		read = rangeRead{err: fmt.Errorf("read commit %s in %s: %w", head, repository, err)}
	}
	r.commits[key] = read
	return read
}

func (r *migrationReader) pullRequests(ctx context.Context, repository, sha string) pullsRead {
	key := commitKey{strings.ToLower(repository), sha}
	if read, ok := r.pulls[key]; ok {
		return read
	}
	pulls, err := r.source.PullRequests(ctx, repository, sha)
	read := pullsRead{pulls: pulls}
	if err != nil {
		read = pullsRead{err: fmt.Errorf("read pull requests of %s in %s: %w", sha, repository, err)}
	}
	r.pulls[key] = read
	return read
}
