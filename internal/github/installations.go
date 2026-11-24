package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type installationEntry struct {
	id        int64
	fetchedAt time.Time
}

func validateRepository(owner, repo string) error {
	if !ownerPattern.MatchString(owner) {
		return fmt.Errorf("%w: owner %q", ErrInvalidRepository, owner)
	}
	if repo == "." || repo == ".." || !repoPattern.MatchString(repo) {
		return fmt.Errorf("%w: repository %q", ErrInvalidRepository, repo)
	}
	return nil
}

func installationKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

func (a *App) installationID(ctx context.Context, owner, repo string) (int64, error) {
	key := installationKey(owner, repo)
	a.mu.Lock()
	entry, ok := a.installations[key]
	a.mu.Unlock()
	if ok && a.now().Sub(entry.fetchedAt) < installationCacheTTL {
		return entry.id, nil
	}

	var installation struct {
		ID          int64   `json:"id"`
		SuspendedAt *string `json:"suspended_at"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	err := a.appRequest(ctx, http.MethodGet, path, &installation)
	switch {
	case hasStatus(err, http.StatusNotFound):
		a.forgetRepository(key)
		return 0, fmt.Errorf("%s/%s: %w", owner, repo, ErrNotInstalled)
	case err != nil:
		return 0, fmt.Errorf("look up installation for %s/%s: %w", owner, repo, err)
	case installation.ID <= 0:
		return 0, fmt.Errorf("look up installation for %s/%s: response has no installation id", owner, repo)
	case installation.SuspendedAt != nil:
		a.forgetRepository(key)
		return 0, fmt.Errorf("%s/%s: installation %d: %w", owner, repo, installation.ID, ErrInstallationSuspended)
	}

	a.mu.Lock()
	a.installations[key] = installationEntry{id: installation.ID, fetchedAt: a.now()}
	a.mu.Unlock()
	return installation.ID, nil
}

func (a *App) forgetRepository(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.installations, key)
}

func (a *App) forgetInstallation(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, entry := range a.installations {
		if entry.id == id {
			delete(a.installations, key)
		}
	}
	delete(a.tokens, id)
}
