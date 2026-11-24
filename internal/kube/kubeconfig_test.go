package kube

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

const kubeconfigPath = "/home/dev/.kube/config"

func kubeconfig(server, clusterExtra, user string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
preferences: {}
current-context: dev
clusters:
- name: dev
  cluster:
    server: %s
%s
- name: other
  cluster:
    server: https://10.0.0.2:6443
contexts:
- name: dev
  context:
    cluster: dev
    user: dev
    namespace: argocd
- name: other
  context:
    cluster: other
    user: other
- name: proxy
  context:
    cluster: dev
- name: dangling-cluster
  context:
    cluster: missing
    user: dev
- name: dangling-user
  context:
    cluster: dev
    user: missing
users:
- name: dev
  user:
%s
- name: other
  user:
    token: other-token
`, server, clusterExtra, user)
}

func TestLoadKubeconfig(t *testing.T) {
	t.Parallel()

	ca := newAuthority(t, "cluster")
	caData := base64.StdEncoding.EncodeToString(ca.pem)
	certPEM, keyPEM := ca.issue(t, "doorstep", x509.ExtKeyUsageClientAuth)
	certData := base64.StdEncoding.EncodeToString(certPEM)
	keyData := base64.StdEncoding.EncodeToString(keyPEM)
	files := map[string]string{
		"/home/dev/.kube/ca.crt":      string(ca.pem),
		"/home/dev/.kube/token":       "file-token\n",
		"/home/dev/.kube/client.crt":  string(certPEM),
		"/home/dev/.kube/client.key":  string(keyPEM),
		"/etc/kubernetes/abs-token":   "absolute-token",
		"/home/dev/.kube/garbage.crt": "not a certificate",
	}
	withCA := "    certificate-authority-data: " + caData

	tests := []struct {
		name       string
		content    string
		context    string
		wantServer string
		wantToken  string
		wantCert   bool
		wantCA     bool
		wantSNI    string
		wantIs     error
		wantErr    string
	}{
		{
			name:       "inline token and CA",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    token: inline-token"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "inline-token",
			wantCA:     true,
		},
		{
			name:       "named context",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    token: inline-token"),
			context:    "other",
			wantServer: "https://10.0.0.2:6443",
			wantToken:  "other-token",
		},
		{
			name:       "relative CA file and token file",
			content:    kubeconfig("https://10.0.0.1:6443", "    certificate-authority: ca.crt", "    tokenFile: token"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "file-token",
			wantCA:     true,
		},
		{
			name:       "absolute token file",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    tokenFile: /etc/kubernetes/abs-token"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "absolute-token",
			wantCA:     true,
		},
		{
			name:       "inline token wins over token file",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    token: inline-token\n    tokenFile: token"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "inline-token",
			wantCA:     true,
		},
		{
			name:       "client certificate data",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    client-certificate-data: "+certData+"\n    client-key-data: "+keyData),
			wantServer: "https://10.0.0.1:6443",
			wantCert:   true,
			wantCA:     true,
		},
		{
			name:       "client certificate files",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    client-certificate: client.crt\n    client-key: /home/dev/.kube/client.key"),
			wantServer: "https://10.0.0.1:6443",
			wantCert:   true,
			wantCA:     true,
		},
		{
			name:       "tls server name",
			content:    kubeconfig("https://10.0.0.1:6443", withCA+"\n    tls-server-name: kubernetes.default", "    token: t"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "t",
			wantCA:     true,
			wantSNI:    "kubernetes.default",
		},
		{
			name:       "kubectl proxy without a user",
			content:    kubeconfig("http://127.0.0.1:8001", "", "    token: unused"),
			context:    "proxy",
			wantServer: "http://127.0.0.1:8001",
		},
		{
			name:       "null exec is ignored",
			content:    kubeconfig("https://10.0.0.1:6443", withCA, "    exec: null\n    token: t"),
			wantServer: "https://10.0.0.1:6443",
			wantToken:  "t",
			wantCA:     true,
		},
		{
			name:    "exec plugin",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: aws"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "kubectl proxy",
		},
		{
			name:    "auth provider",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    auth-provider:\n      name: gcp"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "auth-provider",
		},
		{
			name:    "basic auth",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    username: admin\n    password: hunter2"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "basic authentication",
		},
		{
			name:    "impersonation",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    token: t\n    as: someone"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "impersonation",
		},
		{
			name:    "insecure skip verify",
			content: kubeconfig("https://10.0.0.1:6443", "    insecure-skip-tls-verify: true", "    token: t"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "insecure-skip-tls-verify",
		},
		{
			name:    "proxy url",
			content: kubeconfig("https://10.0.0.1:6443", withCA+"\n    proxy-url: socks5://localhost:1080", "    token: t"),
			wantIs:  ErrUnsupportedAuth,
			wantErr: "proxy-url",
		},
		{
			name:    "plain http to a remote host",
			content: kubeconfig("http://10.0.0.1:8080", "", "    token: t"),
			wantIs:  ErrInvalidConfig,
			wantErr: "loopback",
		},
		{
			name:    "no server",
			content: kubeconfig("", withCA, "    token: t"),
			wantIs:  ErrInvalidConfig,
			wantErr: "has no server",
		},
		{
			name:    "no current context",
			content: strings.Replace(kubeconfig("https://10.0.0.1:6443", withCA, "    token: t"), "current-context: dev", "", 1),
			wantIs:  ErrInvalidConfig,
			wantErr: "no current-context",
		},
		{
			name:    "unknown context",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    token: t"),
			context: "prod",
			wantIs:  ErrInvalidConfig,
			wantErr: `context "prod" not found`,
		},
		{
			name:    "dangling cluster",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    token: t"),
			context: "dangling-cluster",
			wantIs:  ErrInvalidConfig,
			wantErr: `cluster "missing"`,
		},
		{
			name:    "dangling user",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    token: t"),
			context: "dangling-user",
			wantIs:  ErrInvalidConfig,
			wantErr: `user "missing"`,
		},
		{
			name:    "certificate without key",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    client-certificate-data: "+certData),
			wantIs:  ErrInvalidConfig,
			wantErr: "both a client certificate and a client key",
		},
		{
			name:    "mismatched key pair",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    client-certificate: garbage.crt\n    client-key: client.key"),
			wantIs:  ErrInvalidConfig,
			wantErr: "client certificate",
		},
		{
			name:    "bad base64",
			content: kubeconfig("https://10.0.0.1:6443", "    certificate-authority-data: '!!!'", "    token: t"),
			wantIs:  ErrInvalidConfig,
			wantErr: "base64",
		},
		{
			name:    "missing CA file",
			content: kubeconfig("https://10.0.0.1:6443", "    certificate-authority: nope.crt", "    token: t"),
			wantIs:  fs.ErrNotExist,
			wantErr: "/home/dev/.kube/nope.crt",
		},
		{
			name:    "missing token file",
			content: kubeconfig("https://10.0.0.1:6443", withCA, "    tokenFile: nope"),
			wantIs:  fs.ErrNotExist,
		},
		{
			name:    "not yaml",
			content: "clusters: [",
			wantErr: "parse kubeconfig",
		},
		{
			name:    "too large",
			content: strings.Repeat("#", maxKubeconfigBytes+1),
			wantErr: "byte limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemFiles(files)
			fsys.set(kubeconfigPath, tt.content)
			cfg, err := LoadKubeconfig(kubeconfigPath, tt.context, fsys.read)
			if tt.wantIs != nil || tt.wantErr != "" {
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Fatalf("LoadKubeconfig error = %v, want %v", err, tt.wantIs)
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadKubeconfig error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadKubeconfig: %v", err)
			}
			if cfg.Server != tt.wantServer {
				t.Errorf("Server = %q, want %q", cfg.Server, tt.wantServer)
			}
			if (len(cfg.CAData) > 0) != tt.wantCA {
				t.Errorf("CA set = %v, want %v", len(cfg.CAData) > 0, tt.wantCA)
			}
			if (cfg.ClientCert != nil) != tt.wantCert {
				t.Errorf("client certificate set = %v, want %v", cfg.ClientCert != nil, tt.wantCert)
			}
			if cfg.TLSServerName != tt.wantSNI {
				t.Errorf("TLSServerName = %q, want %q", cfg.TLSServerName, tt.wantSNI)
			}
			switch {
			case tt.wantToken == "" && cfg.Token != nil:
				t.Errorf("Token set, want none")
			case tt.wantToken != "":
				if cfg.Token == nil {
					t.Fatal("Token not set")
				}
				if got, err := cfg.Token(); err != nil || got != tt.wantToken {
					t.Errorf("Token() = %q, %v, want %q", got, err, tt.wantToken)
				}
			}
			if _, err := New(cfg, Options{}); err != nil {
				t.Errorf("New: %v", err)
			}
		})
	}
}

func TestLoadKubeconfigMissingFile(t *testing.T) {
	t.Parallel()

	_, err := LoadKubeconfig("/nowhere", "", newMemFiles(nil).read)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadKubeconfig error = %v", err)
	}
}

func TestKubeconfigTokenFileIsReadPerCall(t *testing.T) {
	t.Parallel()

	fsys := newMemFiles(map[string]string{
		kubeconfigPath:          kubeconfig("https://10.0.0.1:6443", "", "    tokenFile: token"),
		"/home/dev/.kube/token": "first",
	})
	cfg, err := LoadKubeconfig(kubeconfigPath, "", fsys.read)
	if err != nil {
		t.Fatalf("LoadKubeconfig: %v", err)
	}
	fsys.set("/home/dev/.kube/token", "second")
	if got, err := cfg.Token(); err != nil || got != "second" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
}
