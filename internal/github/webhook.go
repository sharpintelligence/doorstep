package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	SignatureHeader = "X-Hub-Signature-256"
	EventHeader     = "X-GitHub-Event"
	DeliveryHeader  = "X-GitHub-Delivery"
	signaturePrefix = "sha256="
)

func VerifySignature(secret, body []byte, header string) error {
	if len(secret) == 0 || header == "" {
		return ErrMissingSignature
	}
	digest, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok || len(digest) != hex.EncodedLen(sha256.Size) {
		return ErrInvalidSignature
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}
