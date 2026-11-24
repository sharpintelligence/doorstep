package github

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

func generateRSAKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("GenerateKey(%d): %v", bits, err)
	}
	return key
}

func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func pkcs1PEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestParsePrivateKey(t *testing.T) {
	t.Parallel()

	key := generateRSAKey(t, 2048)
	smallKey := generateRSAKey(t, 1024)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	pkcs1 := pkcs1PEM(key)

	tests := []struct {
		name    string
		input   []byte
		wantErr string
	}{
		{name: "PKCS #1", input: pkcs1},
		{name: "PKCS #8", input: pkcs8PEM(t, key)},
		{name: "escaped newlines", input: []byte(strings.ReplaceAll(string(pkcs1), "\n", `\n`))},
		{name: "surrounding whitespace", input: append(append([]byte("\n  \n"), pkcs1...), "\n\n"...)},
		{name: "PKCS #8 ECDSA key", input: pkcs8PEM(t, ecKey), wantErr: "want an RSA key"},
		{name: "1024 bit key", input: pkcs1PEM(smallKey), wantErr: "has 1024 bits, want at least 2048"},
		{name: "garbage", input: []byte("not a key"), wantErr: "no PEM block found"},
		{name: "empty", input: nil, wantErr: "no PEM block found"},
		{
			name:    "wrong PEM type",
			input:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
			wantErr: `unsupported PEM block type "CERTIFICATE"`,
		},
		{
			name:    "corrupt PKCS #1 body",
			input:   pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
			wantErr: "parse PKCS #1 key",
		},
		{
			name:    "corrupt PKCS #8 body",
			input:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
			wantErr: "parse PKCS #8 key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePrivateKey(tt.input)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParsePrivateKey: %v", err)
				}
				if !got.Equal(key) {
					t.Error("ParsePrivateKey returned a different key")
				}
				return
			}
			if !errors.Is(err, ErrInvalidPrivateKey) {
				t.Fatalf("ParsePrivateKey error = %v, want ErrInvalidPrivateKey", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "MII") {
				t.Errorf("error %q leaks key material", err)
			}
		})
	}
}
