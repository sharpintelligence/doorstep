package kube

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	maxKubeconfigBytes = 1 << 20
	proxyHint          = "run `kubectl proxy` and point a kubeconfig at http://127.0.0.1:8001 instead"
)

type kubeconfigFile struct {
	CurrentContext string             `yaml:"current-context"`
	Clusters       []namedKubeCluster `yaml:"clusters"`
	Contexts       []namedKubeContext `yaml:"contexts"`
	Users          []namedKubeUser    `yaml:"users"`
}

type namedKubeCluster struct {
	Name    string      `yaml:"name"`
	Cluster kubeCluster `yaml:"cluster"`
}

type namedKubeContext struct {
	Name    string      `yaml:"name"`
	Context kubeContext `yaml:"context"`
}

type namedKubeUser struct {
	Name string   `yaml:"name"`
	User kubeUser `yaml:"user"`
}

type kubeCluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	TLSServerName            string `yaml:"tls-server-name"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	ProxyURL                 string `yaml:"proxy-url"`
}

type kubeContext struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type kubeUser struct {
	Token                 string    `yaml:"token"`
	TokenFile             string    `yaml:"tokenFile"`
	ClientCertificate     string    `yaml:"client-certificate"`
	ClientCertificateData string    `yaml:"client-certificate-data"`
	ClientKey             string    `yaml:"client-key"`
	ClientKeyData         string    `yaml:"client-key-data"`
	Username              string    `yaml:"username"`
	Password              string    `yaml:"password"`
	Exec                  yaml.Node `yaml:"exec"`
	AuthProvider          yaml.Node `yaml:"auth-provider"`
	As                    string    `yaml:"as"`
	AsUID                 string    `yaml:"as-uid"`
	AsGroups              []string  `yaml:"as-groups"`
}

func LoadKubeconfig(path, contextName string, readFile func(string) ([]byte, error)) (Config, error) {
	data, err := readFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read kubeconfig: %w", err)
	}
	if len(data) > maxKubeconfigBytes {
		return Config{}, fmt.Errorf("kubeconfig %s is larger than the %d byte limit", path, maxKubeconfigBytes)
	}
	var file kubeconfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return Config{}, fmt.Errorf("parse kubeconfig %s: %w", path, err)
	}
	cfg, err := file.config(contextName, filepath.Dir(path), readFile)
	if err != nil {
		return Config{}, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

func (f kubeconfigFile) config(contextName, dir string, readFile func(string) ([]byte, error)) (Config, error) {
	if contextName == "" {
		contextName = f.CurrentContext
	}
	if contextName == "" {
		return Config{}, fmt.Errorf("%w: no current-context is set; choose a context", ErrInvalidConfig)
	}
	kctx, ok := find(f.Contexts, contextName, func(c namedKubeContext) string { return c.Name })
	if !ok {
		return Config{}, fmt.Errorf("%w: context %q not found", ErrInvalidConfig, contextName)
	}
	cluster, ok := find(f.Clusters, kctx.Context.Cluster, func(c namedKubeCluster) string { return c.Name })
	if !ok {
		return Config{}, fmt.Errorf("%w: cluster %q of context %q not found", ErrInvalidConfig, kctx.Context.Cluster, contextName)
	}
	cfg, err := cluster.Cluster.config(cluster.Name, dir, readFile)
	if err != nil {
		return Config{}, err
	}
	if kctx.Context.User == "" {
		return cfg, nil
	}
	user, ok := find(f.Users, kctx.Context.User, func(u namedKubeUser) string { return u.Name })
	if !ok {
		return Config{}, fmt.Errorf("%w: user %q of context %q not found", ErrInvalidConfig, kctx.Context.User, contextName)
	}
	if err := user.User.apply(&cfg, user.Name, dir, readFile); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c kubeCluster) config(name, dir string, readFile func(string) ([]byte, error)) (Config, error) {
	switch {
	case c.InsecureSkipTLSVerify:
		return Config{}, fmt.Errorf("%w: cluster %q sets insecure-skip-tls-verify; doorstep always verifies the API server certificate, so set certificate-authority-data or %s", ErrUnsupportedAuth, name, proxyHint)
	case c.ProxyURL != "":
		return Config{}, fmt.Errorf("%w: cluster %q sets proxy-url; %s", ErrUnsupportedAuth, name, proxyHint)
	case c.Server == "":
		return Config{}, fmt.Errorf("%w: cluster %q has no server", ErrInvalidConfig, name)
	}
	if _, err := parseServer(c.Server); err != nil {
		return Config{}, err
	}
	ca, err := fileOrData(c.CertificateAuthority, c.CertificateAuthorityData, dir, readFile)
	if err != nil {
		return Config{}, fmt.Errorf("cluster %q certificate authority: %w", name, err)
	}
	return Config{Server: c.Server, CAData: ca, TLSServerName: c.TLSServerName}, nil
}

func (u kubeUser) apply(cfg *Config, name, dir string, readFile func(string) ([]byte, error)) error {
	switch {
	case present(u.Exec):
		return fmt.Errorf("%w: user %q uses an exec credential plugin, which doorstep never runs; %s", ErrUnsupportedAuth, name, proxyHint)
	case present(u.AuthProvider):
		return fmt.Errorf("%w: user %q uses an auth-provider; %s", ErrUnsupportedAuth, name, proxyHint)
	case u.Username != "" || u.Password != "":
		return fmt.Errorf("%w: user %q uses basic authentication; use a token or client certificate, or %s", ErrUnsupportedAuth, name, proxyHint)
	case u.As != "" || u.AsUID != "" || len(u.AsGroups) > 0:
		return fmt.Errorf("%w: user %q uses impersonation; bind the permissions doorstep needs to the user directly", ErrUnsupportedAuth, name)
	}
	switch {
	case u.Token != "":
		cfg.Token = StaticToken(u.Token)
	case u.TokenFile != "":
		cfg.Token = TokenFile(resolve(dir, u.TokenFile), readFile)
		if _, err := cfg.Token(); err != nil {
			return fmt.Errorf("user %q: %w", name, err)
		}
	}
	cert, err := fileOrData(u.ClientCertificate, u.ClientCertificateData, dir, readFile)
	if err != nil {
		return fmt.Errorf("user %q client certificate: %w", name, err)
	}
	key, err := fileOrData(u.ClientKey, u.ClientKeyData, dir, readFile)
	if err != nil {
		return fmt.Errorf("user %q client key: %w", name, err)
	}
	switch {
	case cert == nil && key == nil:
		return nil
	case cert == nil || key == nil:
		return fmt.Errorf("%w: user %q must set both a client certificate and a client key", ErrInvalidConfig, name)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return fmt.Errorf("%w: user %q client certificate: %w", ErrInvalidConfig, name, err)
	}
	cfg.ClientCert = &pair
	return nil
}

func fileOrData(file, data, dir string, readFile func(string) ([]byte, error)) ([]byte, error) {
	if data != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
		if err != nil {
			return nil, fmt.Errorf("%w: data is not valid base64", ErrInvalidConfig)
		}
		return decoded, nil
	}
	if file == "" {
		return nil, nil
	}
	path := resolve(dir, file)
	b, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxCredentialBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxCredentialBytes)
	}
	return b, nil
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

func present(node yaml.Node) bool {
	return node.Kind != 0 && node.ShortTag() != "!!null"
}

func find[T any](items []T, name string, key func(T) string) (T, bool) {
	for _, item := range items {
		if key(item) == name {
			return item, true
		}
	}
	var zero T
	return zero, false
}
