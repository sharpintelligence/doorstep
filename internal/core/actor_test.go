package core

import "testing"

func TestActor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		actor   Actor
		want    string
		wantErr []string
	}{
		{name: "user", actor: Actor{Kind: ActorUser, ID: "octocat", Name: "The Octocat"}, want: "user:octocat"},
		{name: "token", actor: Actor{Kind: ActorToken, ID: "ci"}, want: "token:ci"},
		{name: "cli", actor: Actor{Kind: ActorCLI, ID: "root"}, want: "cli:root"},
		{name: "system", actor: Actor{Kind: ActorSystem}, want: "system"},
		{name: "system with id", actor: Actor{Kind: ActorSystem, ID: "poller"}, want: "system:poller"},
		{name: "id with colon", actor: Actor{Kind: ActorUser, ID: "a:b"}, want: "user:a:b"},
		{name: "missing id", actor: Actor{Kind: ActorUser}, want: "user", wantErr: []string{"actor id must not be empty"}},
		{name: "unknown kind", actor: Actor{Kind: "robot", ID: "r2"}, want: "robot:r2", wantErr: []string{`unknown actor kind "robot"`}},
		{name: "empty kind", actor: Actor{ID: "x"}, want: ":x", wantErr: []string{`unknown actor kind ""`}},
		{name: "padded id", actor: Actor{Kind: ActorUser, ID: " octocat"}, want: "user: octocat", wantErr: []string{"surrounding whitespace"}},
		{name: "control character", actor: Actor{Kind: ActorUser, ID: "octo\ncat"}, want: "user:octo\ncat", wantErr: []string{"control characters"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.actor.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			assertError(t, tt.actor.Validate(), tt.wantErr, nil)
		})
	}
}

func TestActorKinds(t *testing.T) {
	t.Parallel()

	seen := map[ActorKind]bool{}
	for _, k := range ActorKinds() {
		if seen[k] || !k.Valid() {
			t.Errorf("actor kind %q is duplicated or invalid", k)
		}
		seen[k] = true
	}
	for _, k := range []ActorKind{"", "User", "bot"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
}
