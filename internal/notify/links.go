package notify

import "github.com/sharpintelligence/doorstep/internal/core"

type Links struct {
	DeploymentURL func(core.DeploymentID) string
	CommitURL     func(repository, sha string) string
}

func (l Links) deployment(d *core.Deployment) string {
	if d == nil || d.ID == 0 || l.DeploymentURL == nil {
		return ""
	}
	return l.DeploymentURL(d.ID)
}

func (l Links) commit(c core.Commit) string {
	if c.Repository == "" || c.SHA == "" || l.CommitURL == nil {
		return ""
	}
	return l.CommitURL(c.Repository, c.SHA)
}
