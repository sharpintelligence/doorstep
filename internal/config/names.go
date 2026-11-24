package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	maxDNSLabel     = 63
	maxDNSSubdomain = 253
	maxGitHubOwner  = 39
	maxGitHubRepo   = 100
)

func isLowerAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func isAlnum(r rune) bool {
	return isLowerAlnum(r) || (r >= 'A' && r <= 'Z')
}

func isDNSLabel(s string) bool {
	if s == "" || len(s) > maxDNSLabel || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !isLowerAlnum(r) && r != '-' {
			return false
		}
	}
	return true
}

func ValidateAppName(s string) error {
	return validateName(s)
}

func validateName(s string) error {
	if s == "" {
		return errors.New("is required")
	}
	if !isDNSLabel(s) {
		return fmt.Errorf("%q must be a DNS label: lowercase letters, digits and '-', starting and ending with a letter or digit, at most %d characters", s, maxDNSLabel)
	}
	return nil
}

func validateDNSSubdomain(s string) error {
	if s == "" {
		return errors.New("is required")
	}
	if len(s) > maxDNSSubdomain {
		return fmt.Errorf("must be at most %d characters", maxDNSSubdomain)
	}
	for label := range strings.SplitSeq(s, ".") {
		if !isDNSLabel(label) {
			return fmt.Errorf("%q must be a DNS subdomain: lowercase letters, digits, '-' and '.'", s)
		}
	}
	return nil
}

func validateRepo(r RepoName) error {
	if r == "" {
		return errors.New("is required")
	}
	owner, name, ok := strings.Cut(string(r), "/")
	if !ok || strings.Contains(name, "/") {
		return fmt.Errorf("%q must have the form owner/name", r)
	}
	if !isGitHubOwner(owner) {
		return fmt.Errorf("owner %q must be letters, digits and single '-' not at the start or end, at most %d characters", owner, maxGitHubOwner)
	}
	if !isGitHubRepoName(name) {
		return fmt.Errorf("name %q must be letters, digits, '.', '_' and '-', at most %d characters", name, maxGitHubRepo)
	}
	return nil
}

func isGitHubOwner(s string) bool {
	if s == "" || len(s) > maxGitHubOwner || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) && r != '-' {
			return false
		}
	}
	return true
}

func isGitHubRepoName(s string) bool {
	if s == "" || len(s) > maxGitHubRepo || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) && !strings.ContainsRune("._-", r) {
			return false
		}
	}
	return true
}

func validateBranch(s string) error {
	switch {
	case s == "":
		return errors.New("is required")
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "-"), strings.HasSuffix(s, "/"),
		strings.HasSuffix(s, "."), strings.HasSuffix(s, ".lock"),
		strings.Contains(s, ".."), strings.Contains(s, "//"), strings.Contains(s, "@{"):
		return fmt.Errorf("%q is not a valid branch name", s)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("~^:?*[\\", r) {
			return fmt.Errorf("%q is not a valid branch name", s)
		}
	}
	return nil
}

func validateRelativePath(p string) error {
	if path.IsAbs(p) {
		return errors.New("must be relative to the repository root")
	}
	for segment := range strings.SplitSeq(p, "/") {
		if segment == ".." {
			return errors.New("must not contain '..'")
		}
	}
	return nil
}

func validateImageRepository(s string) error {
	if s == "" {
		return errors.New("is required")
	}
	if strings.Contains(s, "@") {
		return fmt.Errorf("%q must not include a digest", s)
	}
	host, repoPath, ok := strings.Cut(s, "/")
	if !ok || !isRegistryHost(host) {
		return fmt.Errorf("%q must start with a registry host, such as ghcr.io/owner/app", s)
	}
	if strings.Contains(repoPath, ":") {
		return fmt.Errorf("%q must not include a tag", s)
	}
	for component := range strings.SplitSeq(repoPath, "/") {
		if !isImagePathComponent(component) {
			return fmt.Errorf("%q must have a lowercase path of letters, digits, '.', '_' and '-'", s)
		}
	}
	return nil
}

func isRegistryHost(host string) bool {
	name, port, hasPort := strings.Cut(host, ":")
	if name == "" || (!hasPort && !strings.Contains(name, ".") && name != "localhost") {
		return false
	}
	for _, r := range name {
		if !isAlnum(r) && r != '.' && r != '-' {
			return false
		}
	}
	if hasPort {
		if port == "" {
			return false
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func isImagePathComponent(s string) bool {
	if s == "" {
		return false
	}
	runes := []rune(s)
	if !isLowerAlnum(runes[0]) || !isLowerAlnum(runes[len(runes)-1]) {
		return false
	}
	for _, r := range runes {
		if !isLowerAlnum(r) && !strings.ContainsRune("._-", r) {
			return false
		}
	}
	return true
}

func validateImageName(s string) error {
	if s == "" {
		return errors.New("is required")
	}
	if strings.Contains(s, "@") {
		return fmt.Errorf("%q must not include a digest", s)
	}
	if strings.ContainsFunc(s, unicode.IsSpace) {
		return fmt.Errorf("%q must not contain whitespace", s)
	}
	last := s[strings.LastIndex(s, "/")+1:]
	if strings.Contains(last, ":") {
		return fmt.Errorf("%q must not include a tag", s)
	}
	return nil
}

func validateEnvName(s string) error {
	for i, r := range s {
		if (r != '_' && !isAlnum(r)) || (i == 0 && r >= '0' && r <= '9') {
			return fmt.Errorf("%q is not a valid environment variable name", s)
		}
	}
	return nil
}

func validateSecretFile(s string) error {
	if !filepath.IsAbs(s) {
		return fmt.Errorf("%q must be an absolute path", s)
	}
	return nil
}

func validateURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use http or https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	if u.User != nil {
		return errors.New("must not include credentials; use url_env or url_file")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must not include a query string or fragment, which often carry tokens; use url_env or url_file")
	}
	return nil
}
