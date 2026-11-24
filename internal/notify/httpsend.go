package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	httpTimeout      = 10 * time.Second
	maxResponseBytes = 4 << 10
	maxExcerptBytes  = 256

	maxRetryAfterSeconds = 3600
)

var errInvalidEndpoint = errors.New("endpoint must be an absolute http or https URL with a host")

func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		transport = t.Clone()
	}
	return &http.Client{
		Transport: transport,
		Timeout:   httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type httpSender struct {
	client *http.Client
	now    func() time.Time
}

func newHTTPSender(client *http.Client, now func() time.Time) httpSender {
	if client == nil {
		client = NewHTTPClient()
	}
	if now == nil {
		now = time.Now
	}
	return httpSender{client: client, now: now}
}

func (s httpSender) post(ctx context.Context, rawURL string, header http.Header, body []byte) error {
	endpoint, err := parseEndpoint(rawURL)
	if err != nil {
		return permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return permanent(errInvalidEndpoint)
	}
	req.Header = header.Clone()
	resp, err := s.client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	return &StatusError{
		StatusCode: resp.StatusCode,
		Excerpt:    excerpt(data),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), s.now()),
	}
}

func parseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return nil, errInvalidEndpoint
	}
	return u, nil
}

func redactURL(err error) error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}
	return fmt.Errorf("%s request: %w", strings.ToLower(urlErr.Op), urlErr.Err)
}

func excerpt(data []byte) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(data), ""))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= maxExcerptBytes {
		return text
	}
	cut := maxExcerptBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(min(seconds, maxRetryAfterSeconds)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
