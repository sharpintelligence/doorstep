package argocd

import (
	"net/url"
	"strings"
)

func normaliseRepo(repoURL string) string {
	raw := strings.TrimSpace(repoURL)
	var path string
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git":
			path = u.Path
		default:
			return ""
		}
	} else if at := strings.Index(raw, "@"); at >= 0 && !strings.Contains(raw, "://") {
		_, rest, ok := strings.Cut(raw[at+1:], ":")
		if !ok {
			return ""
		}
		path = rest
	} else {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return strings.ToLower(owner + "/" + name)
}
