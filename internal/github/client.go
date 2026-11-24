package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	baseURL *url.URL
	http    *http.Client
}

func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	req, err := newRequest(ctx, c.baseURL, method, path, in)
	if err != nil {
		return err
	}
	return send(c.http, req, out)
}

func (c *Client) HTTPClient() *http.Client {
	return c.http
}

func newRequest(ctx context.Context, baseURL *url.URL, method, path string, in any) (*http.Request, error) {
	rawPath, query, _ := strings.Cut(path, "?")
	u := baseURL.JoinPath(rawPath)
	u.RawQuery = query
	body := io.Reader(http.NoBody)
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s request: %w", method, rawPath, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func send(client *http.Client, req *http.Request, out any) error {
	_, err := sendForHeader(client, req, out)
	return err
}

func sendForHeader(client *http.Client, req *http.Request, out any) (http.Header, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newAPIError(resp, body)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, body)
		return resp.Header, nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return nil, fmt.Errorf("decode %s %s response: %w", req.Method, req.URL.Path, err)
	}
	return resp.Header, nil
}
