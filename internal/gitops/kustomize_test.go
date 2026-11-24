package gitops

import (
	"errors"
	"testing"
)

func TestKustomizeWrite(t *testing.T) {
	t.Parallel()

	a, b := string(digestA), string(digestB)
	cases := []editCase{
		{
			name: "replaces the digest and keeps comments, blank lines and other entries",
			src: "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
				"kind: Kustomization\n" +
				"\n" +
				"# images pinned by doorstep\n" +
				"images:\n" +
				"  - name: ghcr.io/acme/blog   # the blog\n" +
				"    newTag: v1\n" +
				"    digest: " + a + "   # pinned\n" +
				"\n" +
				"  - name: ghcr.io/acme/other\n" +
				"    digest: " + a + "\n" +
				"resources:\n" +
				"- deployment.yaml\n",
			value: b,
			want: "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
				"kind: Kustomization\n" +
				"\n" +
				"# images pinned by doorstep\n" +
				"images:\n" +
				"  - name: ghcr.io/acme/blog   # the blog\n" +
				"    newTag: v1\n" +
				"    digest: " + b + "   # pinned\n" +
				"\n" +
				"  - name: ghcr.io/acme/other\n" +
				"    digest: " + a + "\n" +
				"resources:\n" +
				"- deployment.yaml\n",
		},
		{
			name:  "inserts the digest after a leading name and leaves newTag alone",
			src:   "images:\n- name: ghcr.io/acme/blog\n  newTag: v1\nresources: [a.yaml]\n",
			value: b,
			want:  "images:\n- name: ghcr.io/acme/blog\n  digest: " + b + "\n  newTag: v1\nresources: [a.yaml]\n",
		},
		{
			name:  "inserts the digest after a name that is not the first key",
			src:   "images:\n    - newName: registry.local/blog\n      name: ghcr.io/acme/blog # source\n      newTag: \"1.0\"\n",
			value: b,
			want:  "images:\n    - newName: registry.local/blog\n      name: ghcr.io/acme/blog # source\n      digest: " + b + "\n      newTag: \"1.0\"\n",
		},
		{
			name:  "inserts after a quoted name key",
			src:   "images:\n- \"name\": 'ghcr.io/acme/blog'\n",
			value: b,
			want:  "images:\n- \"name\": 'ghcr.io/acme/blog'\n  digest: " + b + "\n",
		},
		{
			name:  "inserts at the end of a file without a trailing newline",
			src:   "images:\n- name: ghcr.io/acme/blog",
			value: b,
			want:  "images:\n- name: ghcr.io/acme/blog\n  digest: " + b,
		},
		{
			name:  "keeps CRLF line endings when inserting",
			src:   "images:\r\n- name: ghcr.io/acme/blog\r\n  newTag: v1\r\n",
			value: b,
			want:  "images:\r\n- name: ghcr.io/acme/blog\r\n  digest: " + b + "\r\n  newTag: v1\r\n",
		},
		{
			name:  "keeps CRLF line endings when replacing",
			src:   "images:\r\n- name: ghcr.io/acme/blog\r\n  digest: " + a + "\r\n",
			value: b,
			want:  "images:\r\n- name: ghcr.io/acme/blog\r\n  digest: " + b + "\r\n",
		},
		{
			name:  "keeps CRLF when the last line has no newline",
			src:   "images:\r\n- name: ghcr.io/acme/blog",
			value: b,
			want:  "images:\r\n- name: ghcr.io/acme/blog\r\n  digest: " + b,
		},
		{
			name:  "keeps double quotes",
			src:   "images:\n- name: ghcr.io/acme/blog\n  digest: \"" + a + "\"\n",
			value: b,
			want:  "images:\n- name: ghcr.io/acme/blog\n  digest: \"" + b + "\"\n",
		},
		{
			name:  "keeps single quotes on an empty placeholder",
			src:   "images:\n- name: ghcr.io/acme/blog\n  digest: ''\n",
			value: b,
			want:  "images:\n- name: ghcr.io/acme/blog\n  digest: '" + b + "'\n",
		},
		{
			name:  "replaces inside a flow mapping",
			src:   "images: [{name: ghcr.io/acme/blog, digest: \"" + a + "\"}]\n",
			value: b,
			want:  "images: [{name: ghcr.io/acme/blog, digest: \"" + b + "\"}]\n",
		},
		{
			name:  "skips a byte order mark",
			src:   "\xef\xbb\xbfimages:\n- name: ghcr.io/acme/blog\n  digest: " + a + "\n",
			value: b,
			want:  "\xef\xbb\xbfimages:\n- name: ghcr.io/acme/blog\n  digest: " + b + "\n",
		},
		{
			name:    "missing entry",
			src:     "images:\n- name: ghcr.io/acme/other\n",
			value:   b,
			wantErr: ErrPinNotFound,
		},
		{
			name:    "missing images list",
			src:     "resources:\n- a.yaml\n",
			value:   b,
			wantErr: ErrPinNotFound,
		},
		{
			name:    "empty images list",
			src:     "images:\n",
			value:   b,
			wantErr: ErrPinNotFound,
		},
		{
			name:    "duplicate entries",
			src:     "images:\n- name: ghcr.io/acme/blog\n- name: ghcr.io/acme/blog\n  newTag: v2\n",
			value:   b,
			wantErr: ErrAmbiguousPin,
		},
		{
			name:    "duplicate digest keys",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest: " + a + "\n  digest: " + a + "\n",
			value:   b,
			wantErr: ErrAmbiguousPin,
		},
		{
			name:    "flow mapping without a digest",
			src:     "images:\n- {name: ghcr.io/acme/blog, newTag: v1}\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "block scalar digest",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest: |\n    " + a + "\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "alias digest",
			src:     "x: &d " + a + "\nimages:\n- name: ghcr.io/acme/blog\n  digest: *d\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "anchored digest",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest: &d " + a + "\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "aliased entry",
			src:     "base: &e {name: ghcr.io/acme/blog}\nimages:\n- *e\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "null digest",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest:\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "tagged digest",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest: !!str " + a + "\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "merge key",
			src:     "base: &b {newTag: v1}\nimages:\n- name: ghcr.io/acme/blog\n  <<: *b\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "images is a mapping",
			src:     "images:\n  name: ghcr.io/acme/blog\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "multi-line name",
			src:     "images:\n- name: \"ghcr.io/acme/\\\n    blog\"\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "invalid yaml",
			src:     "images: [\n",
			value:   b,
			wantErr: ErrUnsupportedYAML,
		},
		{
			name:    "value that needs escaping",
			src:     "images:\n- name: ghcr.io/acme/blog\n  digest: x\n",
			value:   "a: b",
			wantErr: ErrUnsupportedYAML,
		},
	}
	runEditCases(t, kustomizeEditor{image: blogImage}, cases)
}

func TestKustomizeRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     string
		want    string
		wantErr error
	}{
		{name: "digest", src: "images:\n- name: ghcr.io/acme/blog\n  digest: " + string(digestA) + "\n", want: string(digestA)},
		{name: "no digest", src: "images:\n- name: ghcr.io/acme/blog\n  newTag: v1\n", want: ""},
		{name: "null digest", src: "images:\n- name: ghcr.io/acme/blog\n  digest:\n", want: ""},
		{name: "placeholder", src: "images:\n- name: ghcr.io/acme/blog\n  digest: \"\"\n", want: ""},
		{name: "missing", src: "images: []\n", wantErr: ErrPinNotFound},
		{name: "digest is a list", src: "images:\n- name: ghcr.io/acme/blog\n  digest: [a]\n", wantErr: ErrUnsupportedYAML},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readValue(kustomizeEditor{image: blogImage}, []byte(tt.src))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("readValue() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("readValue() = %q, want %q", got, tt.want)
			}
		})
	}
}
