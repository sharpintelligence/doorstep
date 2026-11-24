package github

import (
	"fmt"
	"net/http"
)

type installationTransport struct {
	app            *App
	installationID int64
	base           http.RoundTripper
}

func (t *installationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameOrigin(req.URL, t.app.baseURL) {
		closeRequestBody(req)
		return nil, fmt.Errorf("%w: %s://%s", ErrForeignHost, req.URL.Scheme, req.URL.Host)
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

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
