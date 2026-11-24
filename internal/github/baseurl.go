package github

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

const DefaultBaseURL = "https://api.github.com"

func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBaseURL, err)
	}
	switch {
	case !u.IsAbs() || u.Opaque != "" || u.Host == "":
		return nil, fmt.Errorf("%w: %q must be an absolute URL with a host", ErrInvalidBaseURL, raw)
	case u.User != nil:
		return nil, fmt.Errorf("%w: must not contain user information", ErrInvalidBaseURL)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("%w: %q must not have a query", ErrInvalidBaseURL, raw)
	case u.Fragment != "":
		return nil, fmt.Errorf("%w: %q must not have a fragment", ErrInvalidBaseURL, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: %q must use https unless the host is loopback", ErrInvalidBaseURL, raw)
		}
	default:
		return nil, fmt.Errorf("%w: %q has unsupported scheme %q", ErrInvalidBaseURL, raw, u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return u, nil
}

func WebBaseURL(api *url.URL) *url.URL {
	host := strings.ToLower(api.Hostname())
	switch {
	case host == "api.github.com":
		return &url.URL{Scheme: "https", Host: "github.com"}
	case strings.HasPrefix(host, "api.") && strings.HasSuffix(host, ".ghe.com"):
		return &url.URL{Scheme: api.Scheme, Host: api.Host[len("api."):]}
	}
	return &url.URL{
		Scheme: api.Scheme,
		Host:   api.Host,
		Path:   strings.TrimSuffix(strings.TrimRight(api.Path, "/"), "/api/v3"),
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
