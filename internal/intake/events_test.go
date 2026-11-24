package intake

import (
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var eventSHA = strings.Repeat("c", 40)

func TestParsePush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    Push
		wantOK  bool
		wantErr string
	}{
		{
			name:   "branch push",
			body:   `{"ref":"refs/heads/main","after":"` + eventSHA + `","repository":{"full_name":"acme/web"},"sender":{"login":"octocat"},"pusher":{"name":"Octo Cat"}}`,
			want:   Push{Repository: "acme/web", Branch: "main", SHA: eventSHA, Source: core.SourceWebhook, PushedBy: "octocat"},
			wantOK: true,
		},
		{
			name:   "nested branch name",
			body:   `{"ref":"refs/heads/release/2026","after":"` + eventSHA + `","repository":{"full_name":"acme/web"},"pusher":{"name":"octocat"}}`,
			want:   Push{Repository: "acme/web", Branch: "release/2026", SHA: eventSHA, Source: core.SourceWebhook, PushedBy: "octocat"},
			wantOK: true,
		},
		{
			name:   "no sender or pusher",
			body:   `{"ref":"refs/heads/main","after":"` + eventSHA + `","repository":{"full_name":"acme/web"}}`,
			want:   Push{Repository: "acme/web", Branch: "main", SHA: eventSHA, Source: core.SourceWebhook},
			wantOK: true,
		},
		{name: "tag", body: `{"ref":"refs/tags/v1","after":"` + eventSHA + `","repository":{"full_name":"acme/web"}}`},
		{name: "other ref", body: `{"ref":"refs/notes/commits","after":"` + eventSHA + `","repository":{"full_name":"acme/web"}}`},
		{name: "deleted", body: `{"ref":"refs/heads/main","after":"` + eventSHA + `","deleted":true,"repository":{"full_name":"acme/web"}}`},
		{name: "zero SHA", body: `{"ref":"refs/heads/main","after":"` + zeroSHA + `","repository":{"full_name":"acme/web"}}`},
		{name: "invalid JSON", body: `[`, wantErr: "decode push payload"},
		{name: "JSON array", body: `[]`, wantErr: "decode push payload"},
		{name: "missing repository", body: `{"ref":"refs/heads/main","after":"` + eventSHA + `"}`, wantErr: "missing repository full_name"},
		{name: "repository without owner", body: `{"ref":"refs/heads/main","after":"` + eventSHA + `","repository":{"full_name":"web"}}`, wantErr: "must be owner/name"},
		{name: "uppercase SHA", body: `{"ref":"refs/heads/main","after":"` + strings.ToUpper(eventSHA) + `","repository":{"full_name":"acme/web"}}`, wantErr: "push payload"},
		{name: "missing SHA", body: `{"ref":"refs/heads/main","repository":{"full_name":"acme/web"}}`, wantErr: "push payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := parsePush([]byte(tt.body))
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("parsePush error = %v, want %q", err, tt.wantErr)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parsePush = %+v, %t, want %+v, %t", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseCommitEvent(t *testing.T) {
	t.Parallel()

	repo := `"repository":{"full_name":"acme/web"}`
	tests := []struct {
		name    string
		event   string
		body    string
		want    commitRef
		wantErr string
	}{
		{name: "check run", event: EventCheckRun, body: `{"check_run":{"head_sha":"` + eventSHA + `"},` + repo + `}`, want: commitRef{Repository: "acme/web", SHA: eventSHA}},
		{name: "check suite", event: EventCheckSuite, body: `{"check_suite":{"head_sha":"` + eventSHA + `"},` + repo + `}`, want: commitRef{Repository: "acme/web", SHA: eventSHA}},
		{name: "status", event: EventStatus, body: `{"sha":"` + eventSHA + `",` + repo + `}`, want: commitRef{Repository: "acme/web", SHA: eventSHA}},
		{name: "check run reads only its own SHA", event: EventCheckRun, body: `{"sha":"` + eventSHA + `",` + repo + `}`, wantErr: "check_run payload"},
		{name: "invalid check run JSON", event: EventCheckRun, body: `{`, wantErr: "decode check_run payload"},
		{name: "invalid check suite JSON", event: EventCheckSuite, body: `{`, wantErr: "decode check_suite payload"},
		{name: "invalid status JSON", event: EventStatus, body: `{`, wantErr: "decode status payload"},
		{name: "missing repository", event: EventStatus, body: `{"sha":"` + eventSHA + `"}`, wantErr: "missing repository full_name"},
		{name: "short SHA", event: EventStatus, body: `{"sha":"abc",` + repo + `}`, wantErr: "status payload"},
		{name: "unsupported event", event: EventPush, body: `{}`, wantErr: "does not name a commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCommitEvent(tt.event, []byte(tt.body))
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("parseCommitEvent error = %v, want %q", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseCommitEvent = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseInstallation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		event   string
		body    string
		want    installationChange
		wantErr string
	}{
		{name: "installation", event: EventInstallation, body: `{"action":"deleted","installation":{"id":9}}`, want: installationChange{ID: 9}},
		{
			name:  "installation ignores repositories_removed",
			event: EventInstallation,
			body:  `{"installation":{"id":9},"repositories_removed":[{"full_name":"acme/web"}]}`,
			want:  installationChange{ID: 9},
		},
		{
			name:  "repositories removed",
			event: EventInstallationRepositories,
			body:  `{"installation":{"id":9},"repositories_removed":[{"full_name":"acme/web"},{"full_name":"acme/api"}]}`,
			want: installationChange{ID: 9, Removed: []repositoryName{
				{FullName: "acme/web", Owner: "acme", Name: "web"},
				{FullName: "acme/api", Owner: "acme", Name: "api"},
			}},
		},
		{name: "repositories added", event: EventInstallationRepositories, body: `{"installation":{"id":9},"repositories_added":[{"full_name":"acme/web"}]}`, want: installationChange{ID: 9}},
		{name: "missing id", event: EventInstallation, body: `{"installation":{}}`, wantErr: "missing installation id"},
		{name: "negative id", event: EventInstallation, body: `{"installation":{"id":-1}}`, wantErr: "missing installation id"},
		{name: "invalid JSON", event: EventInstallation, body: `nope`, wantErr: "decode installation payload"},
		{
			name:    "invalid removed repository",
			event:   EventInstallationRepositories,
			body:    `{"installation":{"id":9},"repositories_removed":[{"full_name":"acme/web"},{"full_name":""}]}`,
			wantErr: "repositories_removed[1]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseInstallation(tt.event, []byte(tt.body))
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("parseInstallation error = %v, want %q", err, tt.wantErr)
			}
			if got.ID != tt.want.ID || !slices.Equal(got.Removed, tt.want.Removed) {
				t.Errorf("parseInstallation = %+v, want %+v", got, tt.want)
			}
		})
	}
}
