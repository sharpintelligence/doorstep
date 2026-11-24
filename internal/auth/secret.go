package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

const (
	TokenPrefix   = "dsp_"
	secretBytes   = 32
	csrfMessage   = "doorstep-csrf"
	encodedSecret = 43
)

func newSecret() (raw string, hash [32]byte) {
	b := make([]byte, secretBytes)
	_, _ = rand.Read(b)
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashSecret(raw)
}

func newAPIToken() (raw string, hash [32]byte) {
	secret, _ := newSecret()
	raw = TokenPrefix + secret
	return raw, hashSecret(raw)
}

func hashSecret(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}

func wellFormedSecret(raw string) bool {
	if len(raw) != encodedSecret {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	return err == nil
}

func wellFormedToken(raw string) bool {
	secret, ok := strings.CutPrefix(raw, TokenPrefix)
	return ok && wellFormedSecret(secret)
}

func csrfTokenFor(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	_, _ = mac.Write([]byte(csrfMessage))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func equalSecrets(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
