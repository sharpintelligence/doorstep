package gitops

import (
	"errors"
	"testing"
)

func TestYAMLPathWrite(t *testing.T) {
	t.Parallel()

	a, b := string(digestA), string(digestB)
	ref := blogImage + "@" + b
	manifest := "apiVersion: v1\n" +
		"kind: Service\n" +
		"metadata: {name: blog}\n" +
		"---\n" +
		"apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"spec:\n" +
		"  template:\n" +
		"    spec:\n" +
		"      containers:\n" +
		"        - name: blog\n" +
		"          image: ghcr.io/acme/blog:1.0 # app\n" +
		"        - name: sidecar\n" +
		"          image: ghcr.io/acme/sidecar:2\n"

	tests := []struct {
		name  string
		path  []string
		cases []editCase
	}{
		{
			name: "helm values",
			path: []string{"image", "digest"},
			cases: []editCase{
				{
					name: "placeholder keeps quotes and comment",
					src: "# values for blog\n\nimage:\n  repository: ghcr.io/acme/blog\n  digest: \"\"  # set by doorstep\n\n" +
						"replicas: 2\n",
					value: b,
					want: "# values for blog\n\nimage:\n  repository: ghcr.io/acme/blog\n  digest: \"" + b + "\"  # set by doorstep\n\n" +
						"replicas: 2\n",
				},
				{
					name:  "plain digest",
					src:   "image:\n  digest: " + a + "\n",
					value: b,
					want:  "image:\n  digest: " + b + "\n",
				},
				{
					name:  "double quotes with escapes",
					src:   "image:\n  digest: \"old\\\"value\\u0041\" # c\n",
					value: b,
					want:  "image:\n  digest: \"" + b + "\" # c\n",
				},
				{
					name:  "single quotes with an escaped quote",
					src:   "image:\n  digest: 'it''s' # c\n",
					value: b,
					want:  "image:\n  digest: '" + b + "' # c\n",
				},
				{
					name:  "CRLF",
					src:   "image:\r\n  digest: \"\"\r\nother: 1\r\n",
					value: b,
					want:  "image:\r\n  digest: \"" + b + "\"\r\nother: 1\r\n",
				},
				{
					name:  "lone carriage returns",
					src:   "image:\r  digest: x\rother: 1\r",
					value: b,
					want:  "image:\r  digest: " + b + "\rother: 1\r",
				},
				{
					name:  "non-ASCII before the value",
					src:   "ünïcødé: 1\nimage: {ñame: x, digest: \"\"}\n",
					value: b,
					want:  "ünïcødé: 1\nimage: {ñame: x, digest: \"" + b + "\"}\n",
				},
				{
					name:  "byte order mark",
					src:   "\xef\xbb\xbfimage: {digest: x}\n",
					value: b,
					want:  "\xef\xbb\xbfimage: {digest: " + b + "}\n",
				},
				{name: "missing key", src: "image:\n  repository: x\n", value: b, wantErr: ErrPinNotFound},
				{name: "missing parent", src: "other: 1\n", value: b, wantErr: ErrPinNotFound},
				{name: "parent is a scalar", src: "image: x\n", value: b, wantErr: ErrPinNotFound},
				{name: "empty file", src: "", value: b, wantErr: ErrPinNotFound},
				{name: "null value", src: "image:\n  digest:\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "tilde value", src: "image:\n  digest: ~\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "integer value", src: "image:\n  digest: 5\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "mapping value", src: "image:\n  digest: {a: 1}\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "folded value", src: "image:\n  digest: >\n    x\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "multi-line plain value", src: "image:\n  digest: abc\n    def\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "multi-line quoted value", src: "image:\n  digest: \"abc\n    def\"\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "alias on the path", src: "base: &b {digest: x}\nimage: *b\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "alias value", src: "d: &d x\nimage: {digest: *d}\n", value: b, wantErr: ErrUnsupportedYAML},
				{name: "duplicate key", src: "image:\n  digest: x\n  digest: y\n", value: b, wantErr: ErrAmbiguousPin},
				{name: "same path in two documents", src: "image: {digest: x}\n---\nimage: {digest: y}\n", value: b, wantErr: ErrAmbiguousPin},
			},
		},
		{
			name: "plain manifest",
			path: []string{"spec", "template", "spec", "containers", "0", "image"},
			cases: []editCase{
				{
					name:  "reference in the second document",
					src:   manifest,
					value: ref,
					want: "apiVersion: v1\n" +
						"kind: Service\n" +
						"metadata: {name: blog}\n" +
						"---\n" +
						"apiVersion: apps/v1\n" +
						"kind: Deployment\n" +
						"spec:\n" +
						"  template:\n" +
						"    spec:\n" +
						"      containers:\n" +
						"        - name: blog\n" +
						"          image: " + ref + " # app\n" +
						"        - name: sidecar\n" +
						"          image: ghcr.io/acme/sidecar:2\n",
				},
				{name: "index out of range", src: "spec: {template: {spec: {containers: []}}}\n", value: ref, wantErr: ErrPinNotFound},
			},
		},
		{
			name: "non-numeric segment on a sequence",
			path: []string{"items", "first"},
			cases: []editCase{
				{name: "not found", src: "items: [a, b]\n", value: b, wantErr: ErrPinNotFound},
			},
		},
		{
			name: "numeric key on a mapping",
			path: []string{"ports", "8080"},
			cases: []editCase{
				{name: "mapping key", src: "ports:\n  \"8080\": x\n", value: b, want: "ports:\n  \"8080\": " + b + "\n"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runEditCases(t, yamlPathEditor{path: tt.path}, tt.cases)
		})
	}
}

func TestYAMLPathRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     string
		want    string
		wantErr error
	}{
		{name: "string", src: "image: {digest: " + string(digestA) + "}\n", want: string(digestA)},
		{name: "placeholder", src: "image: {digest: ''}\n", want: ""},
		{name: "null", src: "image: {digest: null}\n", want: ""},
		{name: "number", src: "image: {digest: 1}\n", want: ""},
		{name: "sequence", src: "image: {digest: [1]}\n", wantErr: ErrUnsupportedYAML},
		{name: "missing", src: "image: {}\n", wantErr: ErrPinNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readValue(yamlPathEditor{path: []string{"image", "digest"}}, []byte(tt.src))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("readValue() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("readValue() = %q, want %q", got, tt.want)
			}
		})
	}
}
