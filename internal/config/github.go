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
	EnvGitHubWebhookSecret     = "DOORSTEP_GITHUB_WEBHOOK_SECRET"
	EnvGitHubWebhookSecretFile = "DOORSTEP_GITHUB_WEBHOOK_SECRET_FILE"
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
	AppID             string
	PrivateKey        Secret
	PrivateKeyFile    string
	APIURL            string
	WebhookSecret     Secret
	WebhookSecretFile string
}

func defaultGitHub() GitHub {
	return GitHub{APIURL: github.DefaultBaseURL}
}

func (g GitHub) Enabled() bool {
	return g.AppID != "" || g.PrivateKey != "" || g.PrivateKeyFile != ""
}

func (g GitHub) WebhooksEnabled() bool {
	return g.WebhookSecret != "" || g.WebhookSecretFile != ""
}

func (g GitHub) ReadWebhookSecret(readFile func(string) ([]byte, error)) ([]byte, error) {
	if !g.WebhooksEnabled() {
		return nil, nil
	}
	secret := string(g.WebhookSecret)
	source := EnvGitHubWebhookSecret
	if g.WebhookSecretFile != "" {
		data, err := readFile(g.WebhookSecretFile)
		if err != nil {
			return nil, fmt.Errorf("read github webhook secret: %w", err)
		}
		secret = trimNewline(string(data))
		source = "file " + g.WebhookSecretFile
	}
	if secret == "" {
		return nil, fmt.Errorf("github webhook secret: %s is empty", source)
	}
	return []byte(secret), nil
}

func githubFromEnv(base GitHub, lookup func(string) (string, bool)) GitHub {
	g := base
	if v, ok := lookup(EnvGitHubAppID); ok {
		g.AppID = v
	}
	key, keySet := lookup(EnvGitHubAppPrivateKey)
	keyFile, keyFileSet := lookup(EnvGitHubAppPrivateKeyFile)
	if keySet || keyFileSet {
		g.PrivateKey = Secret(key)
		g.PrivateKeyFile = keyFile
	}
	if v, ok := lookup(EnvGitHubAPIURL); ok {
		g.APIURL = v
	}
	secret, secretSet := lookup(EnvGitHubWebhookSecret)
	secretFile, secretFileSet := lookup(EnvGitHubWebhookSecretFile)
	if secretSet || secretFileSet {
		g.WebhookSecret = Secret(secret)
		g.WebhookSecretFile = secretFile
	}
	return g
}

func (g GitHub) validate() []error {
	var errs []error
	if _, err := github.ParseBaseURL(g.APIURL); err != nil {
		errs = append(errs, fmt.Errorf("github api url: %w", err))
	}
	if g.WebhookSecret != "" && g.WebhookSecretFile != "" {
		errs = append(errs, errors.New("github webhook secret: set only one of "+EnvGitHubWebhookSecretFile+" and "+EnvGitHubWebhookSecret))
	}
	if !g.Enabled() {
		if g.WebhooksEnabled() {
			errs = append(errs, errors.New("github webhook secret: requires the GitHub App ("+EnvGitHubAppID+" and a private key)"))
		}
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
