package kube

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

var (
	serviceAccountCA    = filepath.Join(ServiceAccountDir, "ca.crt")
	serviceAccountToken = filepath.Join(ServiceAccountDir, "token")
)

func envMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestInCluster(t *testing.T) {
	t.Parallel()

	ca := newAuthority(t, "cluster").pem
	tests := []struct {
		name       string
		env        map[string]string
		files      map[string]string
		wantServer string
		wantIs     error
		wantErr    string
	}{
		{
			name:       "service account",
			env:        map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"},
			files:      map[string]string{serviceAccountCA: string(ca), serviceAccountToken: "token-1\n"},
			wantServer: "https://10.96.0.1:443",
		},
		{
			name:       "ipv6 service host",
			env:        map[string]string{EnvServiceHost: "fd00::1", EnvServicePort: "443"},
			files:      map[string]string{serviceAccountCA: string(ca), serviceAccountToken: "token-1"},
			wantServer: "https://[fd00::1]:443",
		},
		{name: "outside a pod", env: map[string]string{}, wantIs: ErrNotInCluster},
		{name: "port missing", env: map[string]string{EnvServiceHost: "10.96.0.1"}, wantIs: ErrNotInCluster},
		{name: "empty host", env: map[string]string{EnvServiceHost: "", EnvServicePort: "443"}, wantIs: ErrNotInCluster},
		{
			name:   "no CA",
			env:    map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"},
			files:  map[string]string{serviceAccountToken: "token-1"},
			wantIs: fs.ErrNotExist,
		},
		{
			name:   "no token",
			env:    map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"},
			files:  map[string]string{serviceAccountCA: string(ca)},
			wantIs: fs.ErrNotExist,
		},
		{
			name:    "empty token",
			env:     map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"},
			files:   map[string]string{serviceAccountCA: string(ca), serviceAccountToken: " \n"},
			wantErr: "is empty",
		},
		{
			name:    "oversized token",
			env:     map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"},
			files:   map[string]string{serviceAccountCA: string(ca), serviceAccountToken: strings.Repeat("a", maxCredentialBytes+1)},
			wantErr: "larger than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := InCluster(envMap(tt.env), newMemFiles(tt.files).read)
			if tt.wantIs != nil || tt.wantErr != "" {
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Fatalf("InCluster error = %v, want %v", err, tt.wantIs)
				}
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Fatalf("InCluster error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("InCluster: %v", err)
			}
			if cfg.Server != tt.wantServer || string(cfg.CAData) != string(ca) || cfg.ClientCert != nil {
				t.Fatalf("config = %+v", cfg)
			}
			if _, err := New(cfg, Options{}); err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

func TestInClusterRereadsToken(t *testing.T) {
	t.Parallel()

	files := newMemFiles(map[string]string{serviceAccountCA: string(newAuthority(t, "cluster").pem), serviceAccountToken: "first"})
	cfg, err := InCluster(envMap(map[string]string{EnvServiceHost: "10.96.0.1", EnvServicePort: "443"}), files.read)
	if err != nil {
		t.Fatalf("InCluster: %v", err)
	}
	for _, want := range []string{"first", "second"} {
		files.set(serviceAccountToken, want+"\n")
		got, err := cfg.Token()
		if err != nil || got != want {
			t.Fatalf("Token() = %q, %v, want %q", got, err, want)
		}
	}
}
