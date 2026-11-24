package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	userPageSize  = 100
	MaxUserPages  = 10
	dotComAPIHost = "api.github.com"
	dotComWebURL  = "https://github.com"
)

type UserOptions struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	UserAgent  string
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

type UserClient struct {
	baseURL   *url.URL
	token     string
	userAgent string
	http      *http.Client
}

func NewUserClient(opts UserOptions) (*UserClient, error) {
	baseURL, err := ParseBaseURL(orDefaultBaseURL(opts.BaseURL))
	if err != nil {
		return nil, err
	}
	if opts.Token == "" {
		return nil, errors.New("github: user access token is required")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	return &UserClient{
		baseURL:   baseURL,
		token:     opts.Token,
		userAgent: userAgent,
		http: &http.Client{
			Transport:     &pinnedTransport{origin: baseURL, base: base},
			CheckRedirect: httpClient.CheckRedirect,
			Timeout:       httpClient.Timeout,
		},
	}, nil
}

func (c *UserClient) User(ctx context.Context) (User, error) {
	req, err := newRequest(ctx, c.baseURL, http.MethodGet, "/user", nil)
	if err != nil {
		return User{}, err
	}
	var u User
	if _, err := c.send(req, &u); err != nil {
		return User{}, err
	}
	if u.ID <= 0 || u.Login == "" {
		return User{}, errors.New("github: /user response has no id or login")
	}
	return u, nil
}

func (c *UserClient) OrgLogins(ctx context.Context) ([]string, error) {
	type membership struct {
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	memberships, err := paginate[membership](ctx, c, "/user/memberships/orgs?state=active")
	if err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(memberships))
	for _, m := range memberships {
		if m.Organization.Login != "" {
			logins = append(logins, m.Organization.Login)
		}
	}
	return logins, nil
}

func (c *UserClient) TeamSlugs(ctx context.Context) ([]string, error) {
	type team struct {
		Slug         string `json:"slug"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	teams, err := paginate[team](ctx, c, "/user/teams")
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(teams))
	for _, t := range teams {
		if t.Slug != "" && t.Organization.Login != "" {
			slugs = append(slugs, t.Organization.Login+"/"+t.Slug)
		}
	}
	return slugs, nil
}

func paginate[T any](ctx context.Context, c *UserClient, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := newRequest(ctx, c.baseURL, http.MethodGet, path+sep+"per_page="+strconv.Itoa(userPageSize), nil)
	if err != nil {
		return nil, err
	}
	var all []T
	for range MaxUserPages {
		var page []T
		header, err := c.send(req, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next := nextLink(header)
		if next == "" {
			return all, nil
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, next, http.NoBody)
		if err != nil {
			return nil, err
		}
	}
	return all, nil
}

func (c *UserClient) send(req *http.Request, out any) (http.Header, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", mediaType)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	return sendForHeader(c.http, req, out)
}

func nextLink(header http.Header) string {
	for _, value := range header.Values("Link") {
		for part := range strings.SplitSeq(value, ",") {
			target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
			if !ok || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for param := range strings.SplitSeq(params, ";") {
				if strings.ReplaceAll(strings.TrimSpace(param), " ", "") == `rel="next"` {
					return strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
				}
			}
		}
	}
	return ""
}

func OAuthEndpoints(apiURL string) (authURL, tokenURL string, err error) {
	u, err := ParseBaseURL(orDefaultBaseURL(apiURL))
	if err != nil {
		return "", "", err
	}
	web := u.Scheme + "://" + u.Host
	if strings.EqualFold(u.Host, dotComAPIHost) {
		web = dotComWebURL
	}
	return web + "/login/oauth/authorize", web + "/login/oauth/access_token", nil
}

func orDefaultBaseURL(raw string) string {
	if raw == "" {
		return DefaultBaseURL
	}
	return raw
}
