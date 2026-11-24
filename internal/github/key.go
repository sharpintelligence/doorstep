package github

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

const minKeyBits = 2048

func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil && bytes.Contains(pemBytes, []byte(`\n`)) {
		block, _ = pem.Decode(bytes.ReplaceAll(pemBytes, []byte(`\n`), []byte("\n")))
	}
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block found", ErrInvalidPrivateKey)
	}
	key, err := parseRSAKey(block)
	if err != nil {
		return nil, err
	}
	if err := checkKeySize(key); err != nil {
		return nil, err
	}
	return key, nil
}

func parseRSAKey(block *pem.Block) (*rsa.PrivateKey, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: parse PKCS #1 key: %w", ErrInvalidPrivateKey, err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: parse PKCS #8 key: %w", ErrInvalidPrivateKey, err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: PKCS #8 key is %T, want an RSA key", ErrInvalidPrivateKey, parsed)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("%w: unsupported PEM block type %q", ErrInvalidPrivateKey, block.Type)
	}
}

func checkKeySize(key *rsa.PrivateKey) error {
	if bits := key.N.BitLen(); bits < minKeyBits {
		return fmt.Errorf("%w: RSA key has %d bits, want at least %d", ErrInvalidPrivateKey, bits, minKeyBits)
	}
	return nil
}
