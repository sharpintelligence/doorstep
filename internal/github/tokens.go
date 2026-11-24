package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type tokenEntry struct {
	value     string
	expiresAt time.Time
}

func (a *App) installationToken(ctx context.Context, id int64) (string, error) {
	if value, ok := a.cachedToken(id); ok {
		return value, nil
	}
	return a.tokenFlights.do(ctx, id, func(ctx context.Context) (string, error) {
		if value, ok := a.cachedToken(id); ok {
			return value, nil
		}
		value, expiresAt, err := a.mintToken(ctx, id)
		if err != nil {
			return "", err
		}
		a.mu.Lock()
		a.tokens[id] = tokenEntry{value: value, expiresAt: expiresAt}
		a.mu.Unlock()
		return value, nil
	})
}

func (a *App) cachedToken(id int64) (string, bool) {
	a.mu.Lock()
	entry, ok := a.tokens[id]
	a.mu.Unlock()
	return entry.value, ok && a.now().Before(entry.expiresAt.Add(-tokenRefreshMargin))
}

func (a *App) mintToken(ctx context.Context, id int64) (string, time.Time, error) {
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(id, 10) + "/access_tokens"
	err := a.appRequest(ctx, http.MethodPost, path, &minted)
	switch {
	case hasStatus(err, http.StatusNotFound):
		a.forgetInstallation(id)
		return "", time.Time{}, fmt.Errorf("installation %d: %w", id, ErrNotInstalled)
	case isSuspended(err):
		a.forgetInstallation(id)
		return "", time.Time{}, fmt.Errorf("installation %d: %w: %w", id, ErrInstallationSuspended, err)
	case err != nil:
		return "", time.Time{}, fmt.Errorf("mint token for installation %d: %w", id, err)
	case minted.Token == "":
		return "", time.Time{}, fmt.Errorf("mint token for installation %d: response has no token", id)
	case minted.ExpiresAt.IsZero():
		return "", time.Time{}, fmt.Errorf("mint token for installation %d: response has no expires_at", id)
	}
	a.logger.DebugContext(ctx, "minted github installation token",
		slog.Int64("installation_id", id),
		slog.Time("expires_at", minted.ExpiresAt),
	)
	return minted.Token, minted.ExpiresAt, nil
}

func isSuspended(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		apiErr.StatusCode == http.StatusForbidden &&
		strings.Contains(strings.ToLower(apiErr.Message), "suspended")
}

func (a *App) invalidateToken(id int64, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry, ok := a.tokens[id]; ok && entry.value == value {
		delete(a.tokens, id)
	}
}
