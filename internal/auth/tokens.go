package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const maxTokenName = 63

var (
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrInvalidTokenName = errors.New("invalid token name")
)

type Tokens struct {
	repo   Repository
	logger *slog.Logger
}

type CreateTokenInput struct {
	Name      string
	Role      core.Role
	ExpiresIn time.Duration
	By        core.Actor
}

func NewTokens(repo Repository, logger *slog.Logger) *Tokens {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Tokens{repo: repo, logger: logger}
}

func ValidateTokenName(name string) error {
	if name == "" || len(name) > maxTokenName {
		return fmt.Errorf("%w: %q must be 1 to %d characters", ErrInvalidTokenName, name, maxTokenName)
	}
	for i, r := range name {
		lowerAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !lowerAlnum && (i == 0 || (r != '.' && r != '_' && r != '-')) {
			return fmt.Errorf("%w: %q must be lowercase letters, digits, '.', '_' and '-', starting with a letter or digit", ErrInvalidTokenName, name)
		}
	}
	return nil
}

func (t *Tokens) Create(ctx context.Context, in CreateTokenInput) (string, store.APIToken, error) {
	var errs []error
	errs = append(errs, ValidateTokenName(in.Name))
	if !in.Role.Valid() {
		errs = append(errs, fmt.Errorf("%w %q", core.ErrUnknownRole, in.Role))
	}
	if in.ExpiresIn < 0 {
		errs = append(errs, fmt.Errorf("expiry must not be negative, got %s", in.ExpiresIn))
	}
	if err := in.By.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("creator: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return "", store.APIToken{}, err
	}

	raw, hash := newAPIToken()
	var created store.APIToken
	err := t.repo.InTx(ctx, func(tx Repository) error {
		var err error
		created, err = tx.CreateAPIToken(ctx, store.NewAPIToken{
			Name:      in.Name,
			Hash:      hash,
			Role:      in.Role,
			CreatedBy: in.By.String(),
			TTL:       in.ExpiresIn,
		})
		if err != nil {
			return err
		}
		detail := map[string]any{"token": created.Name, "role": created.Role}
		if created.ExpiresAt != nil {
			detail["expires_at"] = created.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return tx.RecordAudit(ctx, core.AuditEntry{Actor: in.By, Action: core.AuditTokenCreate, Detail: mustJSON(detail)})
	})
	if err != nil {
		return "", store.APIToken{}, err
	}
	return raw, created, nil
}

func (t *Tokens) List(ctx context.Context) ([]store.APIToken, error) {
	return t.repo.ListAPITokens(ctx)
}

func (t *Tokens) Revoke(ctx context.Context, name string, by core.Actor) error {
	if err := by.Validate(); err != nil {
		return fmt.Errorf("revoker: %w", err)
	}
	return t.repo.InTx(ctx, func(tx Repository) error {
		revoked, err := tx.RevokeAPIToken(ctx, name)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, core.AuditEntry{
			Actor:  by,
			Action: core.AuditTokenRevoke,
			Detail: mustJSON(map[string]any{"token": revoked.Name, "role": revoked.Role}),
		})
	})
}

func (t *Tokens) Authenticate(ctx context.Context, raw string) (Principal, error) {
	if !wellFormedToken(raw) {
		return Principal{}, ErrUnauthenticated
	}
	token, err := t.repo.FindAPITokenByHash(ctx, hashSecret(raw))
	if errors.Is(err, core.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if err := t.repo.TouchAPIToken(ctx, token.ID); err != nil {
		t.logger.WarnContext(ctx, "recording api token use failed", slog.String("token", token.Name), slog.Any("error", err))
	}
	return Principal{
		Actor:  core.Actor{Kind: core.ActorToken, ID: token.Name, Name: token.Name},
		Role:   token.Role,
		Method: MethodToken,
	}, nil
}

func mustJSON(v map[string]any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("auth: encode audit detail: %v", err))
	}
	return data
}
