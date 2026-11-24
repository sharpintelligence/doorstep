package intake

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	EventPush                     = "push"
	EventCheckRun                 = "check_run"
	EventCheckSuite               = "check_suite"
	EventStatus                   = "status"
	EventInstallation             = "installation"
	EventInstallationRepositories = "installation_repositories"
	EventPing                     = "ping"

	branchRefPrefix = "refs/heads/"
)

type repositoryPayload struct {
	FullName string `json:"full_name"`
}

type accountPayload struct {
	Login string `json:"login"`
}

type pushPayload struct {
	Ref        string            `json:"ref"`
	After      string            `json:"after"`
	Deleted    bool              `json:"deleted"`
	Repository repositoryPayload `json:"repository"`
	Sender     accountPayload    `json:"sender"`
	Pusher     struct {
		Name string `json:"name"`
	} `json:"pusher"`
}

type checkRunPayload struct {
	CheckRun struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_run"`
	Repository repositoryPayload `json:"repository"`
}

type checkSuitePayload struct {
	CheckSuite struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_suite"`
	Repository repositoryPayload `json:"repository"`
}

type statusPayload struct {
	SHA        string            `json:"sha"`
	Context    string            `json:"context"`
	Repository repositoryPayload `json:"repository"`
}

type installationPayload struct {
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	RepositoriesRemoved []repositoryPayload `json:"repositories_removed"`
}

type commitRef struct {
	Repository string
	SHA        string
	Context    string
}

type repositoryName struct {
	FullName string
	Owner    string
	Name     string
}

type installationChange struct {
	ID      int64
	Removed []repositoryName
}

func parsePush(body []byte) (Push, bool, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Push{}, false, fmt.Errorf("decode push payload: %w", err)
	}
	if err := validateRepositoryName(p.Repository.FullName); err != nil {
		return Push{}, false, fmt.Errorf("push payload: %w", err)
	}
	branch, ok := strings.CutPrefix(p.Ref, branchRefPrefix)
	if !ok || branch == "" || p.Deleted || p.After == zeroSHA {
		return Push{}, false, nil
	}
	if err := core.ValidateCommitSHA(p.After); err != nil {
		return Push{}, false, fmt.Errorf("push payload: %w", err)
	}
	pushedBy := p.Sender.Login
	if pushedBy == "" {
		pushedBy = p.Pusher.Name
	}
	return Push{
		Repository: p.Repository.FullName,
		Branch:     branch,
		SHA:        p.After,
		Source:     core.SourceWebhook,
		PushedBy:   pushedBy,
	}, true, nil
}

func parseCommitEvent(event string, body []byte) (commitRef, error) {
	var ref commitRef
	switch event {
	case EventCheckRun:
		var p checkRunPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return commitRef{}, fmt.Errorf("decode %s payload: %w", event, err)
		}
		ref = commitRef{Repository: p.Repository.FullName, SHA: p.CheckRun.HeadSHA}
	case EventCheckSuite:
		var p checkSuitePayload
		if err := json.Unmarshal(body, &p); err != nil {
			return commitRef{}, fmt.Errorf("decode %s payload: %w", event, err)
		}
		ref = commitRef{Repository: p.Repository.FullName, SHA: p.CheckSuite.HeadSHA}
	case EventStatus:
		var p statusPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return commitRef{}, fmt.Errorf("decode %s payload: %w", event, err)
		}
		ref = commitRef{Repository: p.Repository.FullName, SHA: p.SHA, Context: p.Context}
	default:
		return commitRef{}, fmt.Errorf("event %q does not name a commit", event)
	}
	if err := validateRepositoryName(ref.Repository); err != nil {
		return commitRef{}, fmt.Errorf("%s payload: %w", event, err)
	}
	if err := core.ValidateCommitSHA(ref.SHA); err != nil {
		return commitRef{}, fmt.Errorf("%s payload: %w", event, err)
	}
	return ref, nil
}

func parseInstallation(event string, body []byte) (installationChange, error) {
	var p installationPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return installationChange{}, fmt.Errorf("decode %s payload: %w", event, err)
	}
	if p.Installation.ID <= 0 {
		return installationChange{}, fmt.Errorf("%s payload: missing installation id", event)
	}
	change := installationChange{ID: p.Installation.ID}
	if event != EventInstallationRepositories {
		return change, nil
	}
	for i, r := range p.RepositoriesRemoved {
		name, err := parseRepositoryName(r.FullName)
		if err != nil {
			return installationChange{}, fmt.Errorf("%s payload: repositories_removed[%d]: %w", event, i, err)
		}
		change.Removed = append(change.Removed, name)
	}
	return change, nil
}

func validateRepositoryName(fullName string) error {
	_, err := parseRepositoryName(fullName)
	return err
}

func parseRepositoryName(fullName string) (repositoryName, error) {
	if fullName == "" {
		return repositoryName{}, errors.New("missing repository full_name")
	}
	owner, name, err := splitRepository(fullName)
	if err != nil {
		return repositoryName{}, err
	}
	return repositoryName{FullName: fullName, Owner: owner, Name: name}, nil
}
