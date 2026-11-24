package github

import (
	"fmt"
	"net/http"
	"net/url"
)

type pinnedTransport struct {
	origin *url.URL
	base   http.RoundTripper
}

func (t *pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkOrigin(req, t.origin); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

type installationTransport struct {
	app            *App
	installationID int64
	base           http.RoundTripper
}

func (t *installationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkOrigin(req, t.app.baseURL); err != nil {
		return nil, err
	}
	token, err := t.app.installationToken(req.Context(), t.installationID)
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}
	authed := req.Clone(req.Context())
	t.app.setHeaders(authed, token)
	resp, err := t.base.RoundTrip(authed)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		t.app.invalidateToken(t.installationID, token)
	}
	return resp, nil
}

func checkOrigin(req *http.Request, origin *url.URL) error {
	if sameOrigin(req.URL, origin) {
		return nil
	}
	closeRequestBody(req)
	return fmt.Errorf("%w: %s://%s", ErrForeignHost, req.URL.Scheme, req.URL.Host)
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
