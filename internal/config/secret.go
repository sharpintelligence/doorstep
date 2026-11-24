package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MaxSecretBytes = 64 << 10

type SecretRef struct {
	Label string
	Value string
	Env   string
	File  string
}

func (s SecretRef) Read(lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (string, error) {
	var value, source string
	switch {
	case s.Value != "":
		return s.Value, nil
	case s.Env != "":
		source = "environment variable " + s.Env
		v, ok := lookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("%s: %s is not set", s.Label, source)
		}
		value = v
	case s.File != "":
		source = "file " + s.File
		data, err := readFile(s.File)
		if err != nil {
			return "", fmt.Errorf("%s: %w", s.Label, err)
		}
		value = string(data)
	default:
		return "", fmt.Errorf("%s: no value, environment variable or file configured", s.Label)
	}
	value = trimNewline(value)
	if value == "" {
		return "", fmt.Errorf("%s: %s is empty", s.Label, source)
	}
	return value, nil
}

func trimNewline(s string) string {
	if trimmed, ok := strings.CutSuffix(s, "\r\n"); ok {
		return trimmed
	}
	return strings.TrimSuffix(s, "\n")
}

func (c Catalog) CheckSecrets(lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) error {
	var errs []error
	for _, ref := range c.SecretRefs() {
		if _, err := ref.Read(lookupEnv, readFile); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ReadSecretFile(name string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSecretBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSecretBytes {
		return nil, fmt.Errorf("file %s is larger than the %d byte limit", name, MaxSecretBytes)
	}
	return data, nil
}
