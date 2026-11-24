package gitops

import (
	"errors"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var (
	digestA = core.Digest("sha256:" + strings.Repeat("a", 64))
	digestB = core.Digest("sha256:" + strings.Repeat("b", 64))
	digestC = core.Digest("sha256:" + strings.Repeat("c", 64))
	digestD = core.Digest("sha256:" + strings.Repeat("d", 64))
)

const (
	blogImage = "ghcr.io/acme/blog"
	appSHA    = "0123456789abcdef0123456789abcdef01234567"
)

type editCase struct {
	name    string
	src     string
	value   string
	want    string
	wantErr error
}

func runEditCases(t *testing.T, e editor, cases []editCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := writeValue(e, []byte(tt.src), tt.value)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("writeValue() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("writeValue() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("writeValue() =\n%q\nwant\n%q", got, tt.want)
			}
			again, err := writeValue(e, got, tt.value)
			if err != nil {
				t.Fatalf("second writeValue() error = %v", err)
			}
			if string(again) != tt.want {
				t.Fatalf("second writeValue() is not idempotent:\n%q", again)
			}
			value, err := readValue(e, got)
			if err != nil || value != tt.value {
				t.Fatalf("readValue() = %q, %v; want %q", value, err, tt.value)
			}
		})
	}
}
