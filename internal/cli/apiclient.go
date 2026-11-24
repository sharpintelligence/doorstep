package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
)

const (
	EnvServerURL = "DOORSTEP_SERVER_URL"
	EnvToken     = "DOORSTEP_TOKEN"
	EnvTokenFile = "DOORSTEP_TOKEN_FILE"

	flagServer       = "server"
	defaultServerURL = "http://127.0.0.1:8080"
	apiTimeout       = 30 * time.Second
	maxResponseBytes = 1 << 20
)

type apiClient struct {
	base      *url.URL
	token     string
	userAgent string
	http      *http.Client
}

type apiError struct {
	Status  int
	Code    string
	Message string
	Body    json.RawMessage
}

func (e *apiError) Error() string {
	return e.Message
}

func addServerFlag(flags *pflag.FlagSet, dst *string) {
	flags.StringVar(dst, flagServer, "",
		"doorstep server URL (env "+EnvServerURL+", default "+defaultServerURL+")")
}

func newAPIClient(changed func(name string) bool, serverFlag string, lookupEnv LookupEnv) (*apiClient, error) {
	raw := defaultServerURL
	if v, ok := lookupEnv(EnvServerURL); ok {
		raw = v
	}
	source := EnvServerURL
	if changed(flagServer) {
		raw, source = serverFlag, "--"+flagServer
	}
	base, err := parseServerURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	token, err := readToken(lookupEnv)
	if err != nil {
		return nil, err
	}
	if token != "" && !protectsToken(base) {
		return nil, fmt.Errorf("%s: refusing to send the API token over plain http to %s; use https, or http only to a loopback address", source, base.Hostname())
	}
	return &apiClient{
		base:      base,
		token:     token,
		userAgent: "doorstep/" + buildinfo.Read().Version,
		http: &http.Client{
			Timeout: apiTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func parseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	switch {
	case err != nil:
		return nil, errors.New("must be a URL such as " + defaultServerURL)
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	case u.Host == "":
		return nil, errors.New("must include a host")
	case u.User != nil:
		return nil, errors.New("must not include user information; use " + EnvToken + " or " + EnvTokenFile)
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("must not include a query or fragment")
	}
	return u, nil
}

func protectsToken(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readToken(lookupEnv LookupEnv) (string, error) {
	token, tokenSet := lookupEnv(EnvToken)
	file, fileSet := lookupEnv(EnvTokenFile)
	switch {
	case tokenSet && fileSet:
		return "", errors.New("set at most one of " + EnvToken + " and " + EnvTokenFile)
	case fileSet:
		data, err := config.ReadSecretFile(file)
		if err != nil {
			return "", fmt.Errorf("%s: %w", EnvTokenFile, err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("%s: file %s is empty", EnvTokenFile, file)
		}
	case tokenSet:
		token = strings.TrimSpace(token)
		if token == "" {
			return "", fmt.Errorf("%s: must not be empty", EnvToken)
		}
	}
	if strings.ContainsFunc(token, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "", errors.New("API token must contain only printable ASCII characters")
	}
	return token, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, in, out any) error {
	return c.request(ctx, method, path, nil, in, out)
}

func (c *apiClient) request(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader = http.NoBody
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.base.Redacted(), unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("response is larger than the %d byte limit", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.responseError(resp, data)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *apiClient) responseError(resp *http.Response, data []byte) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("server answered with a redirect (%s) to %q; set --%s to the final URL", resp.Status, resp.Header.Get("Location"), flagServer)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e := &apiError{Status: resp.StatusCode, Body: data}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		e.Code, e.Message = envelope.Error.Code, envelope.Error.Message
	} else {
		e.Message = "server returned " + resp.Status
	}
	if resp.StatusCode == http.StatusUnauthorized && c.token == "" {
		e.Message += " (set " + EnvToken + " or " + EnvTokenFile + ")"
	}
	return e
}

func unwrapURLError(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}
