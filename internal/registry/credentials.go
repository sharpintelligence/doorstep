package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

const redacted = "[redacted]"

type CredentialSource interface {
	Lookup(host, repository string) (Credential, bool, error)
}

type Credential struct {
	Username      string
	Password      string
	IdentityToken string
	RegistryToken string
}

func (c Credential) String() string {
	fields := []string{fmt.Sprintf("Username: %q", c.Username)}
	for _, secret := range c.secrets() {
		fields = append(fields, secret.name+": "+redacted)
	}
	return "registry.Credential{" + strings.Join(fields, ", ") + "}"
}

func (c Credential) GoString() string {
	return c.String()
}

func (c Credential) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("username", c.Username)}
	for _, secret := range c.secrets() {
		attrs = append(attrs, slog.String(secret.key, redacted))
	}
	return slog.GroupValue(attrs...)
}

type secretField struct {
	name string
	key  string
}

func (c Credential) secrets() []secretField {
	var fields []secretField
	if c.Password != "" {
		fields = append(fields, secretField{name: "Password", key: "password"})
	}
	if c.IdentityToken != "" {
		fields = append(fields, secretField{name: "IdentityToken", key: "identity_token"})
	}
	if c.RegistryToken != "" {
		fields = append(fields, secretField{name: "RegistryToken", key: "registry_token"})
	}
	return fields
}

func (c Credential) authenticator() authn.Authenticator {
	return authn.FromConfig(authn.AuthConfig{
		Username:      c.Username,
		Password:      c.Password,
		IdentityToken: c.IdentityToken,
		RegistryToken: c.RegistryToken,
	})
}

type Credentials struct {
	entries []credentialEntry
}

type credentialEntry struct {
	key        string
	credential Credential
}

func (c Credentials) Len() int {
	return len(c.entries)
}

func (c Credentials) Keys() []string {
	keys := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		keys = append(keys, e.key)
	}
	return keys
}

func (c Credentials) String() string {
	return fmt.Sprintf("registry.Credentials%q", c.Keys())
}

func (c Credentials) GoString() string {
	return c.String()
}

func (c Credentials) LogValue() slog.Value {
	return slog.AnyValue(c.Keys())
}

func (c Credentials) Lookup(host, repository string) (Credential, bool, error) {
	target := canonicalHost(strings.ToLower(host)) + "/" + repository
	best := -1
	for i, e := range c.entries {
		if target != e.key && !strings.HasPrefix(target, e.key+"/") {
			continue
		}
		if best < 0 || len(e.key) > len(c.entries[best].key) {
			best = i
		}
	}
	if best < 0 {
		return Credential{}, false, nil
	}
	return c.entries[best].credential, true, nil
}

type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

func ParseDockerConfig(data []byte) (Credentials, error) {
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Credentials{}, describeJSONError(err)
	}

	rawKeys := make([]string, 0, len(cfg.Auths))
	for k := range cfg.Auths {
		rawKeys = append(rawKeys, k)
	}
	slices.Sort(rawKeys)

	var errs []error
	seen := make(map[string]string, len(rawKeys))
	entries := make([]credentialEntry, 0, len(rawKeys))
	for _, raw := range rawKeys {
		key, err := normalizeKey(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("auths[%q]: %w", raw, err))
			continue
		}
		if first, dup := seen[key]; dup {
			errs = append(errs, fmt.Errorf("auths[%q]: same registry as auths[%q] (%s)", raw, first, key))
			continue
		}
		seen[key] = raw
		cred, err := cfg.Auths[raw].credential()
		if err != nil {
			errs = append(errs, fmt.Errorf("auths[%q]: %w", raw, err))
			continue
		}
		entries = append(entries, credentialEntry{key: key, credential: cred})
	}
	if err := errors.Join(errs...); err != nil {
		return Credentials{}, err
	}
	slices.SortFunc(entries, func(a, b credentialEntry) int { return strings.Compare(a.key, b.key) })
	return Credentials{entries: entries}, nil
}

func describeJSONError(err error) error {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("not valid JSON (error at byte %d)", syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Errorf("not a Docker config: unexpected JSON %s at %q", typeErr.Value, typeErr.Field)
	}
	return errors.New("not a Docker config")
}

func (a dockerAuth) credential() (Credential, error) {
	cred := Credential{
		Username:      a.Username,
		Password:      a.Password,
		IdentityToken: a.IdentityToken,
		RegistryToken: a.RegistryToken,
	}
	if a.Auth != "" {
		user, password, err := decodeAuth(a.Auth)
		if err != nil {
			return Credential{}, err
		}
		cred.Username, cred.Password = user, password
	}
	if cred.IdentityToken == "" && cred.RegistryToken == "" && (cred.Username == "" || cred.Password == "") {
		return Credential{}, errors.New("no usable credential: set auth, username and password, identitytoken or registrytoken")
	}
	return cred, nil
}

func decodeAuth(encoded string) (string, string, error) {
	encoded = strings.TrimSpace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return "", "", errors.New("auth is not valid base64")
	}
	user, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", errors.New("auth must be base64 of username:password")
	}
	if user == "" {
		return "", "", errors.New("auth has an empty username")
	}
	if password == "" {
		return "", "", errors.New("auth has an empty password")
	}
	return user, password, nil
}

func normalizeKey(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	key = strings.TrimPrefix(key, "https://")
	key = strings.TrimPrefix(key, "http://")
	key = strings.TrimRight(key, "/")
	host, path, _ := strings.Cut(key, "/")
	if path == "v1" || path == "v2" {
		key, path = host, ""
	}
	if host == "" {
		return "", errors.New("registry host must not be empty")
	}
	if strings.Contains(key, "//") || strings.ContainsAny(key, " \t*?") {
		return "", errors.New("must be a registry host with an optional repository path prefix")
	}
	host = canonicalHost(host)
	if path == "" {
		return host, nil
	}
	return host + "/" + path, nil
}

func canonicalHost(host string) string {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "index.docker.io"
	}
	return host
}

func LoadDockerConfigFile(path string) (Credentials, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Credentials{}, fmt.Errorf("read registry credentials file: %w", err)
	}
	creds, err := ParseDockerConfig(data)
	if err != nil {
		return Credentials{}, fmt.Errorf("parse registry credentials file %s: %w", path, err)
	}
	return creds, nil
}

type FileCredentials struct {
	Path string
}

func (f FileCredentials) Lookup(host, repository string) (Credential, bool, error) {
	creds, err := LoadDockerConfigFile(f.Path)
	if err != nil {
		return Credential{}, false, err
	}
	return creds.Lookup(host, repository)
}
