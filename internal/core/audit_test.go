package core

import (
	"encoding/json"
	"testing"
)

func TestAuditActionValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		action AuditAction
		want   bool
	}{
		{action: "deploy.request", want: true},
		{action: "freeze.set_reason", want: true},
		{action: "a.b.c", want: true},
		{action: "_._", want: true},
		{action: ""},
		{action: "deploy"},
		{action: "deploy."},
		{action: ".deploy"},
		{action: "deploy..request"},
		{action: "Deploy.request"},
		{action: "deploy.request2"},
		{action: "deploy-request.x"},
		{action: "deploy .request"},
		{action: "deploy.request\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.action), func(t *testing.T) {
			t.Parallel()
			if got := tt.action.Valid(); got != tt.want {
				t.Errorf("%q.Valid() = %v, want %v", tt.action, got, tt.want)
			}
		})
	}
}

func TestAuditActionsAreValidAndUnique(t *testing.T) {
	t.Parallel()

	seen := map[AuditAction]bool{}
	for _, a := range AuditActions() {
		if seen[a] || !a.Valid() {
			t.Errorf("audit action %q is duplicated or invalid", a)
		}
		seen[a] = true
	}
}

func TestAuditEntryValidate(t *testing.T) {
	t.Parallel()

	id := DeploymentID(3)
	valid := func() AuditEntry {
		return AuditEntry{
			Actor:  Actor{Kind: ActorUser, ID: "octocat"},
			Role:   RoleDeployer,
			Action: AuditDeployRequest,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*AuditEntry)
		wantErr []string
		wantIs  error
	}{
		{name: "minimal", mutate: func(*AuditEntry) {}},
		{name: "without role", mutate: func(e *AuditEntry) { e.Role = "" }},
		{name: "full", mutate: func(e *AuditEntry) {
			e.Target = &Target{App: "web", Environment: "prod"}
			e.DeploymentID = &id
			e.Detail = json.RawMessage(` {"reason":"hotfix"}`)
		}},
		{name: "system actor", mutate: func(e *AuditEntry) { e.Actor = Actor{Kind: ActorSystem} }},
		{name: "invalid actor", mutate: func(e *AuditEntry) { e.Actor = Actor{Kind: ActorUser} }, wantErr: []string{"actor: actor id must not be empty"}},
		{name: "unknown role", mutate: func(e *AuditEntry) { e.Role = "root" }, wantErr: []string{`"root"`}, wantIs: ErrUnknownRole},
		{name: "bad action", mutate: func(e *AuditEntry) { e.Action = "Deploy" }, wantErr: []string{`audit action "Deploy"`}},
		{name: "empty target", mutate: func(e *AuditEntry) { e.Target = &Target{} }, wantErr: []string{"target app", "target environment"}},
		{name: "detail array", mutate: func(e *AuditEntry) { e.Detail = json.RawMessage(`[1]`) }, wantErr: []string{"JSON object"}},
		{name: "detail invalid", mutate: func(e *AuditEntry) { e.Detail = json.RawMessage(`{`) }, wantErr: []string{"JSON object"}},
		{name: "detail string", mutate: func(e *AuditEntry) { e.Detail = json.RawMessage(`"x"`) }, wantErr: []string{"JSON object"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := valid()
			tt.mutate(&e)
			assertError(t, e.Validate(), tt.wantErr, tt.wantIs)
		})
	}
}
