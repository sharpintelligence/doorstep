package gitops

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func deployRequest() Request {
	return Request{
		Target: Target{
			App:             "blog",
			Owner:           "acme",
			Repo:            "gitops",
			Branch:          "main",
			Dir:             "apps/blog",
			ImageRepository: blogImage,
			Kustomize:       &KustomizeImage{Name: blogImage},
			Mode:            ModeCommit,
		},
		Digest:     digestB,
		Deployment: 42,
		Kind:       core.KindDeploy,
		AppCommit:  core.Commit{Repository: "acme/blog", SHA: appSHA},
	}
}

func rollbackRequest() Request {
	req := deployRequest()
	req.Kind = core.KindRollback
	of := core.DeploymentID(41)
	req.RollbackOf = &of
	req.Digest = digestA
	req.Blocks = []core.Digest{digestB, digestC}
	return req
}

func TestFormatMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		req         Request
		previous    core.Digest
		url         string
		wantSubject string
		wantBody    string
	}{
		{
			name:        "deploy",
			req:         deployRequest(),
			previous:    digestA,
			wantSubject: "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb",
			wantBody: "Deploy ghcr.io/acme/blog@" + string(digestB) + " to blog, built from acme/blog@" + appSHA + ".\n\n" +
				"Doorstep-App: blog\n" +
				"Doorstep-Deployment: 42\n" +
				"Doorstep-Kind: deploy\n" +
				"Doorstep-App-Commit: acme/blog@" + appSHA + "\n" +
				"Doorstep-Image: ghcr.io/acme/blog\n" +
				"Doorstep-Digest: " + string(digestB) + "\n" +
				"Doorstep-Previous-Digest: " + string(digestA),
		},
		{
			name:        "first deploy with a url",
			req:         deployRequest(),
			url:         "https://doorstep.example/deployments/42",
			wantSubject: "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb",
			wantBody: "Deploy ghcr.io/acme/blog@" + string(digestB) + " to blog, built from acme/blog@" + appSHA + ".\n\n" +
				"Doorstep-App: blog\n" +
				"Doorstep-Deployment: 42\n" +
				"Doorstep-Kind: deploy\n" +
				"Doorstep-App-Commit: acme/blog@" + appSHA + "\n" +
				"Doorstep-Image: ghcr.io/acme/blog\n" +
				"Doorstep-Digest: " + string(digestB) + "\n" +
				"Doorstep-URL: https://doorstep.example/deployments/42",
		},
		{
			name:        "rollback with blocks",
			req:         rollbackRequest(),
			previous:    digestB,
			wantSubject: "rollback(blog): pin ghcr.io/acme/blog@sha256:aaaaaaaaaaaa",
			wantBody: "Roll blog back to ghcr.io/acme/blog@" + string(digestA) + ", reverting deployment 41.\n\n" +
				"Doorstep-App: blog\n" +
				"Doorstep-Deployment: 42\n" +
				"Doorstep-Kind: rollback\n" +
				"Doorstep-App-Commit: acme/blog@" + appSHA + "\n" +
				"Doorstep-Image: ghcr.io/acme/blog\n" +
				"Doorstep-Digest: " + string(digestA) + "\n" +
				"Doorstep-Previous-Digest: " + string(digestB) + "\n" +
				"Doorstep-Rollback-Of: 41\n" +
				"Doorstep-Block: " + string(digestB) + "\n" +
				"Doorstep-Block: " + string(digestC),
		},
		{
			name: "deploy carrying pending unblocks and blocks",
			req: func() Request {
				req := deployRequest()
				req.Blocks = []core.Digest{digestC}
				req.Unblocks = []core.Digest{digestA, digestB}
				return req
			}(),
			wantSubject: "deploy(blog): pin ghcr.io/acme/blog@sha256:bbbbbbbbbbbb",
			wantBody: "Deploy ghcr.io/acme/blog@" + string(digestB) + " to blog, built from acme/blog@" + appSHA + ".\n\n" +
				"Doorstep-App: blog\n" +
				"Doorstep-Deployment: 42\n" +
				"Doorstep-Kind: deploy\n" +
				"Doorstep-App-Commit: acme/blog@" + appSHA + "\n" +
				"Doorstep-Image: ghcr.io/acme/blog\n" +
				"Doorstep-Digest: " + string(digestB) + "\n" +
				"Doorstep-Block: " + string(digestC) + "\n" +
				"Doorstep-Unblock: " + string(digestA) + "\n" +
				"Doorstep-Unblock: " + string(digestB),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subject, body, err := FormatMessage(tt.req, tt.previous, tt.url)
			if err != nil {
				t.Fatalf("FormatMessage() error = %v", err)
			}
			if subject != tt.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tt.wantSubject)
			}
			if body != tt.wantBody {
				t.Errorf("body =\n%s\nwant\n%s", body, tt.wantBody)
			}
		})
	}
}

func TestFormatMessageRejectsInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "newline", url: "https://x\nDoorstep-Unblock: " + string(digestA)},
		{name: "carriage return", url: "https://x\rfoo"},
		{name: "tab", url: "https://x\tfoo"},
		{name: "NUL", url: "https://x\x00"},
		{name: "trailing space", url: "https://x "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := FormatMessage(deployRequest(), "", tt.url); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("FormatMessage() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestParseMessageRoundTrip(t *testing.T) {
	t.Parallel()

	rollbackOf := core.DeploymentID(41)
	tests := []struct {
		name     string
		req      Request
		previous core.Digest
		url      string
		want     Trailers
	}{
		{
			name:     "deploy",
			req:      deployRequest(),
			previous: digestA,
			url:      "https://doorstep.example/d/42",
			want: Trailers{
				App:            "blog",
				Deployment:     42,
				Kind:           core.KindDeploy,
				AppCommit:      core.Commit{Repository: "acme/blog", SHA: appSHA},
				Image:          blogImage,
				Digest:         digestB,
				PreviousDigest: digestA,
				URL:            "https://doorstep.example/d/42",
			},
		},
		{
			name:     "rollback",
			req:      rollbackRequest(),
			previous: digestB,
			want: Trailers{
				App:            "blog",
				Deployment:     42,
				Kind:           core.KindRollback,
				AppCommit:      core.Commit{Repository: "acme/blog", SHA: appSHA},
				Image:          blogImage,
				Digest:         digestA,
				PreviousDigest: digestB,
				RollbackOf:     rollbackOf,
				Blocks:         []core.Digest{digestB, digestC},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subject, body, err := FormatMessage(tt.req, tt.previous, tt.url)
			if err != nil {
				t.Fatalf("FormatMessage() error = %v", err)
			}
			for _, message := range []string{
				subject + "\n\n" + body,
				subject + " (#7)\n\n" + body + "\n",
				strings.ReplaceAll(subject+"\n\n"+body, "\n", "\r\n"),
			} {
				if got := ParseMessage(message); !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("ParseMessage() = %+v\nwant %+v", got, tt.want)
				}
			}
		})
	}
}

func TestParseMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    Trailers
	}{
		{
			name:    "unblock commit",
			message: "unblock(blog): allow sha256:aaaa\n\nDoorstep-App: blog\nDoorstep-Unblock: " + string(digestA) + "\nDoorstep-Unblock: " + string(digestB),
			want:    Trailers{App: "blog", Unblocks: []core.Digest{digestA, digestB}},
		},
		{
			name:    "trailers outside the last paragraph are ignored",
			message: "subject\n\nDoorstep-Deployment: 9\n\nSigned-off-by: someone",
			want:    Trailers{},
		},
		{
			name:    "invalid values are dropped",
			message: "s\n\nDoorstep-Deployment: -3\nDoorstep-Kind: sideways\nDoorstep-Digest: sha256:short\nDoorstep-App-Commit: nope\nDoorstep-Rollback-Of: x\nDoorstep-Block: md5:1",
			want:    Trailers{},
		},
		{
			name:    "no trailers",
			message: "fix typo",
			want:    Trailers{},
		},
		{
			name:    "other trailers are kept apart",
			message: "s\n\nCo-Authored-By: a <a@b>\nDoorstep-Deployment: 7",
			want:    Trailers{Deployment: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseMessage(tt.message); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseMessage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTrailersFormatUnblock(t *testing.T) {
	t.Parallel()

	got, err := Trailers{App: "blog", Unblocks: []core.Digest{digestD}}.Format()
	if err != nil {
		t.Fatalf("Format() error = %v", err)
	}
	if want := "Doorstep-App: blog\nDoorstep-Unblock: " + string(digestD); got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
	if _, err := (Trailers{}).Format(); err == nil {
		t.Fatal("Format() of empty trailers succeeded")
	}
}
