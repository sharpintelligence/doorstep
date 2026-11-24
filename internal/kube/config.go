package kube

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

const (
	EnvServiceHost     = "KUBERNETES_SERVICE_HOST"
	EnvServicePort     = "KUBERNETES_SERVICE_PORT"
	ServiceAccountDir  = "/var/run/secrets/kubernetes.io/serviceaccount"
	maxCredentialBytes = 64 << 10
)

var (
	ErrNotInCluster    = errors.New("not running in a kubernetes pod: " + EnvServiceHost + " and " + EnvServicePort + " are not set")
	ErrInvalidConfig   = errors.New("invalid kubernetes connection configuration")
	ErrUnsupportedAuth = errors.New("unsupported kubeconfig setting")
)

type Config struct {
	Server        string
	CAData        []byte
	TLSServerName string
	Token         func() (string, error)
	ClientCert    *tls.Certificate
}

func InCluster(lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (Config, error) {
	host, hostOK := lookupEnv(EnvServiceHost)
	port, portOK := lookupEnv(EnvServicePort)
	if !hostOK || !portOK || host == "" || port == "" {
		return Config{}, ErrNotInCluster
	}
	ca, err := readFile(filepath.Join(ServiceAccountDir, "ca.crt"))
	if err != nil {
		return Config{}, fmt.Errorf("read service account CA: %w", err)
	}
	cfg := Config{
		Server: (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}).String(),
		CAData: ca,
		Token:  TokenFile(filepath.Join(ServiceAccountDir, "token"), readFile),
	}
	if _, err := cfg.Token(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func TokenFile(path string, readFile func(string) ([]byte, error)) func() (string, error) {
	return func() (string, error) {
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("read kubernetes token file %s: %w", path, err)
		}
		if len(b) > maxCredentialBytes {
			return "", fmt.Errorf("kubernetes token file %s is larger than %d bytes", path, maxCredentialBytes)
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("kubernetes token file %s is empty", path)
		}
		return token, nil
	}
}

func StaticToken(token string) func() (string, error) {
	return func() (string, error) { return token, nil }
}

func parseServer(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: server %q is not a valid URL", ErrInvalidConfig, raw)
	}
	switch {
	case u.Host == "" || u.Opaque != "":
		return nil, fmt.Errorf("%w: server %q must be an absolute URL with a host", ErrInvalidConfig, raw)
	case u.User != nil:
		return nil, fmt.Errorf("%w: server %q must not include user information", ErrInvalidConfig, raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("%w: server %q must not include a query string or fragment", ErrInvalidConfig, raw)
	case u.Scheme == "https":
	case u.Scheme == "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: server %q uses plain http, which is only allowed for loopback hosts such as kubectl proxy", ErrInvalidConfig, raw)
		}
	default:
		return nil, fmt.Errorf("%w: server %q must use https", ErrInvalidConfig, raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
