package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	const secret = "It's a Secret to Everybody"
	const body = "Hello, World!"
	valid := sign(secret, body)
	digest := strings.TrimPrefix(valid, "sha256=")

	tests := []struct {
		name   string
		secret string
		body   string
		header string
		want   error
	}{
		{
			name:   "github documentation example",
			secret: secret,
			body:   body,
			header: "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17",
		},
		{name: "valid", secret: secret, body: body, header: valid},
		{name: "uppercase hex", secret: secret, body: body, header: "sha256=" + strings.ToUpper(digest)},
		{name: "empty body", secret: secret, body: "", header: sign(secret, "")},
		{name: "wrong secret", secret: "other", body: body, header: valid, want: ErrInvalidSignature},
		{name: "tampered body", secret: secret, body: body + "!", header: valid, want: ErrInvalidSignature},
		{name: "missing header", secret: secret, body: body, header: "", want: ErrMissingSignature},
		{name: "empty secret", secret: "", body: body, header: sign("", body), want: ErrMissingSignature},
		{name: "sha1 prefix", secret: secret, body: body, header: "sha1=" + digest, want: ErrInvalidSignature},
		{name: "no prefix", secret: secret, body: body, header: digest, want: ErrInvalidSignature},
		{name: "short digest", secret: secret, body: body, header: valid[:len(valid)-2], want: ErrInvalidSignature},
		{name: "odd length digest", secret: secret, body: body, header: valid[:len(valid)-1], want: ErrInvalidSignature},
		{name: "long digest", secret: secret, body: body, header: valid + "00", want: ErrInvalidSignature},
		{name: "non hex digest", secret: secret, body: body, header: "sha256=" + strings.Repeat("zz", 32), want: ErrInvalidSignature},
		{name: "prefix only", secret: secret, body: body, header: "sha256=", want: ErrInvalidSignature},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := VerifySignature([]byte(tt.secret), []byte(tt.body), tt.header)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("VerifySignature() = %v, want %v", err, tt.want)
			}
		})
	}
}
