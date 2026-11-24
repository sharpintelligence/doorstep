package auth

import (
	"context"
	"strings"
	"unicode"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func UserActor(provider, subject, username string) core.Actor {
	name := username
	if strings.TrimSpace(name) != name || name == "" || strings.ContainsFunc(name, unicode.IsControl) {
		name = subject
	}
	return core.Actor{Kind: core.ActorUser, ID: provider + ":" + subject, Name: name}
}

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
