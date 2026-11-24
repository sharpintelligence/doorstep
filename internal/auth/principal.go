package auth

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Method string

const (
	MethodSession Method = "session"
	MethodToken   Method = "token"
)

type Principal struct {
	Actor  core.Actor
	Role   core.Role
	Method Method
}

func (p Principal) Allows(required core.Role) bool {
	return p.Role.Allows(required)
}

type principalKey struct{}

type csrfKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

func withCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfKey{}, token)
}

func CSRFToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfKey{}).(string)
	return token
}
