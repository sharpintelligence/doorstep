package gitops

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	TrailerApp            = "Doorstep-App"
	TrailerDeployment     = "Doorstep-Deployment"
	TrailerKind           = "Doorstep-Kind"
	TrailerAppCommit      = "Doorstep-App-Commit"
	TrailerImage          = "Doorstep-Image"
	TrailerDigest         = "Doorstep-Digest"
	TrailerPreviousDigest = "Doorstep-Previous-Digest"
	TrailerRollbackOf     = "Doorstep-Rollback-Of"
	TrailerBlock          = "Doorstep-Block"
	TrailerUnblock        = "Doorstep-Unblock"
	TrailerURL            = "Doorstep-URL"

	shortDigestLength = len("sha256:") + 12
)

type Trailers struct {
	App            string
	Deployment     core.DeploymentID
	Kind           core.Kind
	AppCommit      core.Commit
	Image          string
	Digest         core.Digest
	PreviousDigest core.Digest
	RollbackOf     core.DeploymentID
	Blocks         []core.Digest
	Unblocks       []core.Digest
	URL            string
}

type trailer struct {
	key   string
	value string
}

func FormatMessage(req Request, previous core.Digest, url string) (string, string, error) {
	t := Trailers{
		App:            req.Target.App,
		Deployment:     req.Deployment,
		Kind:           req.Kind,
		AppCommit:      req.AppCommit,
		Image:          req.Target.ImageRepository,
		Digest:         req.Digest,
		PreviousDigest: previous,
		Blocks:         req.Blocks,
		Unblocks:       req.Unblocks,
		URL:            url,
	}
	if req.RollbackOf != nil {
		t.RollbackOf = *req.RollbackOf
	}
	trailers, err := t.Format()
	if err != nil {
		return "", "", err
	}
	pinned := req.Target.ImageRepository + "@" + string(req.Digest)
	short := req.Target.ImageRepository + "@" + string(req.Digest)[:min(len(req.Digest), shortDigestLength)]
	subject := "deploy(" + req.Target.App + "): pin " + short
	explanation := "Deploy " + pinned + " to " + req.Target.App + ", built from " + formatAppCommit(req.AppCommit) + "."
	if req.Kind == core.KindRollback {
		subject = "rollback(" + req.Target.App + "): pin " + short
		explanation = "Roll " + req.Target.App + " back to " + pinned + ", reverting deployment " +
			strconv.FormatInt(int64(t.RollbackOf), 10) + "."
	}
	return subject, explanation + "\n\n" + trailers, nil
}

func (t Trailers) Format() (string, error) {
	var list []trailer
	add := func(key, value string) {
		if value != "" {
			list = append(list, trailer{key: key, value: value})
		}
	}
	add(TrailerApp, t.App)
	if t.Deployment != 0 {
		add(TrailerDeployment, strconv.FormatInt(int64(t.Deployment), 10))
	}
	add(TrailerKind, string(t.Kind))
	if t.AppCommit != (core.Commit{}) {
		add(TrailerAppCommit, formatAppCommit(t.AppCommit))
	}
	add(TrailerImage, t.Image)
	add(TrailerDigest, string(t.Digest))
	add(TrailerPreviousDigest, string(t.PreviousDigest))
	if t.RollbackOf != 0 {
		add(TrailerRollbackOf, strconv.FormatInt(int64(t.RollbackOf), 10))
	}
	for _, block := range t.Blocks {
		add(TrailerBlock, string(block))
	}
	for _, unblock := range t.Unblocks {
		add(TrailerUnblock, string(unblock))
	}
	add(TrailerURL, t.URL)
	if len(list) == 0 {
		return "", errors.New("commit message needs at least one trailer")
	}
	lines := make([]string, 0, len(list))
	for _, tr := range list {
		if strings.ContainsFunc(tr.value, unicode.IsControl) || strings.TrimSpace(tr.value) != tr.value {
			return "", fmt.Errorf("%w: trailer %s has a control character or surrounding whitespace", ErrInvalidRequest, tr.key)
		}
		lines = append(lines, tr.key+": "+tr.value)
	}
	return strings.Join(lines, "\n"), nil
}

func ParseMessage(message string) Trailers {
	message = strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), " \t\n")
	if i := strings.LastIndex(message, "\n\n"); i >= 0 {
		message = message[i+2:]
	}
	var t Trailers
	for line := range strings.SplitSeq(message, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case TrailerApp:
			t.App = value
		case TrailerDeployment:
			t.Deployment = parseDeploymentID(value)
		case TrailerKind:
			if kind := core.Kind(value); kind.Valid() {
				t.Kind = kind
			}
		case TrailerAppCommit:
			t.AppCommit = parseAppCommit(value)
		case TrailerImage:
			t.Image = value
		case TrailerDigest:
			t.Digest = parseDigest(value)
		case TrailerPreviousDigest:
			t.PreviousDigest = parseDigest(value)
		case TrailerRollbackOf:
			t.RollbackOf = parseDeploymentID(value)
		case TrailerBlock:
			if d := parseDigest(value); d != "" {
				t.Blocks = append(t.Blocks, d)
			}
		case TrailerUnblock:
			if d := parseDigest(value); d != "" {
				t.Unblocks = append(t.Unblocks, d)
			}
		case TrailerURL:
			t.URL = value
		}
	}
	return t
}

func formatAppCommit(c core.Commit) string {
	return c.Repository + "@" + c.SHA
}

func parseAppCommit(value string) core.Commit {
	i := strings.LastIndex(value, "@")
	if i < 0 {
		return core.Commit{}
	}
	c := core.Commit{Repository: value[:i], SHA: value[i+1:]}
	if c.Validate() != nil || validateRepoName(c.Repository) != nil {
		return core.Commit{}
	}
	return c
}

func parseDeploymentID(value string) core.DeploymentID {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return core.DeploymentID(id)
}

func parseDigest(value string) core.Digest {
	d, err := core.ParseDigest(value)
	if err != nil {
		return ""
	}
	return d
}
