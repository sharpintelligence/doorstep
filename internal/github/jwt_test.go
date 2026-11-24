package github

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestJWT(t *testing.T) {
	t.Parallel()

	key := generateRSAKey(t, 2048)
	clock := newFakeClock()
	tests := []struct {
		name  string
		appID string
	}{
		{name: "numeric app id", appID: "123456"},
		{name: "client id", appID: "Iv23liAbCdEf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, err := New(Options{AppID: tt.appID, PrivateKey: key, Now: clock.Now})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			token, err := app.jwt()
			if err != nil {
				t.Fatalf("jwt: %v", err)
			}
			if strings.Contains(token, "=") {
				t.Errorf("token %q contains padding", token)
			}
			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("token has %d segments, want 3", len(parts))
			}

			signature := decodeBase64(t, parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
				t.Errorf("signature does not verify: %v", err)
			}

			var header map[string]string
			if err := json.Unmarshal(decodeBase64(t, parts[0]), &header); err != nil {
				t.Fatalf("header: %v", err)
			}
			if header["alg"] != "RS256" || header["typ"] != "JWT" || len(header) != 2 {
				t.Errorf("header = %v", header)
			}

			var claims map[string]any
			if err := json.Unmarshal(decodeBase64(t, parts[1]), &claims); err != nil {
				t.Fatalf("claims: %v", err)
			}
			now := clock.Now().Unix()
			want := map[string]any{"iss": tt.appID, "iat": float64(now - 60), "exp": float64(now + 540)}
			if len(claims) != len(want) {
				t.Errorf("claims = %v, want %v", claims, want)
			}
			for k, v := range want {
				if claims[k] != v {
					t.Errorf("claim %s = %v, want %v", k, claims[k], v)
				}
			}
		})
	}
}

func decodeBase64(t *testing.T, segment string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode %q: %v", segment, err)
	}
	return b
}
