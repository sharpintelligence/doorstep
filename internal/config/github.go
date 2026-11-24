package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/sharpintelligence/doorstep/internal/github"
)

const (
	EnvGitHubAppID             = "DOORSTEP_GITHUB_APP_ID"
	EnvGitHubAppPrivateKey     = "DOORSTEP_GITHUB_APP_PRIVATE_KEY"
	EnvGitHubAppPrivateKeyFile = "DOORSTEP_GITHUB_APP_PRIVATE_KEY_FILE"
	EnvGitHubAPIURL            = "DOORSTEP_GITHUB_API_URL"
)

const redacted = "[redacted]"

type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

func (s Secret) GoString() string { return s.String() }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

type GitHub struct {
	AppID          string
	PrivateKey     Secret
	PrivateKeyFile string
	APIURL         string
}

func defaultGitHub() GitHub {
	return GitHub{APIURL: github.DefaultBaseURL}
}

func (g GitHub) Enabled() bool {
	return g.AppID != "" || g.PrivateKey != "" || g.PrivateKeyFile != ""
}

func githubFromEnv(base GitHub, lookup func(string) (string, bool)) GitHub {
	g := base
	if v, ok := lookup(EnvGitHubAppID); ok {
		g.AppID = v
	}
	if v, ok := lookup(EnvGitHubAppPrivateKey); ok {
		g.PrivateKey = Secret(v)
	}
	if v, ok := lookup(EnvGitHubAppPrivateKeyFile); ok {
		g.PrivateKeyFile = v
	}
	if v, ok := lookup(EnvGitHubAPIURL); ok {
		g.APIURL = v
	}
	return g
}

func (g GitHub) validate() []error {
	var errs []error
	if _, err := github.ParseBaseURL(g.APIURL); err != nil {
		errs = append(errs, fmt.Errorf("github api url: %w", err))
	}
	if !g.Enabled() {
		return errs
	}
	switch {
	case g.AppID == "":
		errs = append(errs, errors.New("github app id: required when a private key is set ("+EnvGitHubAppID+")"))
	case strings.ContainsFunc(g.AppID, unicode.IsSpace):
		errs = append(errs, fmt.Errorf("github app id: must not contain whitespace, got %q", g.AppID))
	}
	switch {
	case g.PrivateKey == "" && g.PrivateKeyFile == "":
		errs = append(errs, errors.New("github app private key: required when the app id is set ("+EnvGitHubAppPrivateKeyFile+" or "+EnvGitHubAppPrivateKey+")"))
	case g.PrivateKey != "" && g.PrivateKeyFile != "":
		errs = append(errs, errors.New("github app private key: set only one of "+EnvGitHubAppPrivateKeyFile+" and "+EnvGitHubAppPrivateKey))
	}
	return errs
}
