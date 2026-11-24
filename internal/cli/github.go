package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/github"
)

func newGitHubApp(cfg config.GitHub, readFile func(string) ([]byte, error), logger *slog.Logger) (*github.App, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	keyPEM := []byte(cfg.PrivateKey)
	if cfg.PrivateKeyFile != "" {
		b, err := readFile(cfg.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read github app private key: %w", err)
		}
		keyPEM = b
	}
	key, err := github.ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	return github.New(github.Options{
		AppID:      cfg.AppID,
		PrivateKey: key,
		BaseURL:    cfg.APIURL,
		UserAgent:  userAgent(),
		Logger:     logger,
	})
}

func logGitHubApp(ctx context.Context, logger *slog.Logger, cfg config.GitHub, app *github.App) {
	if app == nil {
		logger.InfoContext(ctx, "github app not configured")
		return
	}
	logger.InfoContext(ctx, "github app configured",
		slog.String("app_id", cfg.AppID),
		slog.String("api_url", cfg.APIURL),
	)
}
