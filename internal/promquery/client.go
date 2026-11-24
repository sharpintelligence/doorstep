package promquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultTimeout   = 10 * time.Second
	MaxResponseBytes = 1 << 20

	maxExcerptBytes = 256
	statusSuccess   = "success"

	ErrorBadData = "bad_data"
)

var (
	ErrInvalidBaseURL    = errors.New("prometheus base URL must be an absolute http or https URL with a host and without credentials, query or fragment")
	ErrResponseTooLarge  = fmt.Errorf("prometheus response is larger than the %d byte limit", MaxResponseBytes)
	ErrMalformedResponse = errors.New("malformed prometheus response")
)

type ResultType string

const (
	ResultScalar ResultType = "scalar"
	ResultVector ResultType = "vector"
	ResultMatrix ResultType = "matrix"
	ResultString ResultType = "string"
)

type Point struct {
	At    time.Time
	Value float64
}

type Series struct {
	Labels map[string]string
	Points []Point
}

type Result struct {
	Type   ResultType
	Series []Series
}

type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("prometheus responded %d %s", e.Status, http.StatusText(e.Status))
	if e.Status >= http.StatusOK && e.Status < http.StatusMultipleChoices {
		msg = "prometheus reported an error"
	}
	if e.Type != "" {
		msg += ": " + e.Type
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	Authorize  func(*http.Request) error
}

type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
	authorize func(*http.Request) error
}

func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		transport = t.Clone()
	}
	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func New(opts Options) (*Client, error) {
	base, err := url.Parse(opts.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, ErrInvalidBaseURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = NewHTTPClient()
	}
	return &Client{base: base, http: client, userAgent: opts.UserAgent, authorize: opts.Authorize}, nil
}

func (c *Client) Query(ctx context.Context, expr string, at time.Time) (Result, error) {
	form := url.Values{"query": {expr}, "time": {formatTime(at)}}
	return c.post(ctx, "api/v1/query", form)
}

func (c *Client) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (Result, error) {
	if step <= 0 {
		return Result{}, fmt.Errorf("query range step %s must be positive", step)
	}
	form := url.Values{
		"query": {expr},
		"start": {formatTime(from)},
		"end":   {formatTime(to)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return c.post(ctx, "api/v1/query_range", form)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func (c *Client) post(ctx context.Context, endpoint string, form url.Values) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.JoinPath(endpoint).String(), strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, fmt.Errorf("build prometheus request: %w", stripURL(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.authorize != nil {
		if err := c.authorize(req); err != nil {
			return Result{}, fmt.Errorf("authorize prometheus request: %w", err)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, stripURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read prometheus response: %w", stripURL(err))
	}
	if len(body) > MaxResponseBytes {
		return Result{}, ErrResponseTooLarge
	}
	return decode(resp.StatusCode, body)
}

func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s prometheus: %w", strings.ToLower(urlErr.Op), urlErr.Err)
	}
	return err
}

type envelope struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

type data struct {
	ResultType ResultType      `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type rawSeries struct {
	Metric map[string]string `json:"metric"`
	Value  json.RawMessage   `json:"value"`
	Values []json.RawMessage `json:"values"`
}

func decode(status int, body []byte) (Result, error) {
	var env envelope
	jsonErr := json.Unmarshal(body, &env)
	ok := status >= http.StatusOK && status < http.StatusMultipleChoices
	switch {
	case !ok && jsonErr == nil && env.Status != "":
		return Result{}, &Error{Status: status, Type: env.ErrorType, Message: env.Error}
	case !ok:
		return Result{}, &Error{Status: status, Message: excerpt(body)}
	case jsonErr != nil:
		return Result{}, fmt.Errorf("%w: not JSON", ErrMalformedResponse)
	case env.Status != statusSuccess:
		return Result{}, &Error{Status: status, Type: env.ErrorType, Message: env.Error}
	}
	var d data
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return Result{}, fmt.Errorf("%w: data: %w", ErrMalformedResponse, err)
	}
	switch d.ResultType {
	case ResultString:
		return Result{Type: ResultString}, nil
	case ResultScalar:
		p, err := decodePoint(d.Result)
		if err != nil {
			return Result{}, err
		}
		return Result{Type: d.ResultType, Series: []Series{{Labels: map[string]string{}, Points: []Point{p}}}}, nil
	case ResultVector, ResultMatrix:
		return decodeSeries(d.ResultType, d.Result)
	default:
		return Result{}, fmt.Errorf("%w: unknown result type %q", ErrMalformedResponse, excerpt([]byte(d.ResultType)))
	}
}

func decodeSeries(t ResultType, raw json.RawMessage) (Result, error) {
	var list []rawSeries
	if err := json.Unmarshal(raw, &list); err != nil {
		return Result{}, fmt.Errorf("%w: %s result: %w", ErrMalformedResponse, t, err)
	}
	out := Result{Type: t, Series: make([]Series, 0, len(list))}
	for _, rs := range list {
		s := Series{Labels: rs.Metric}
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		if t == ResultVector {
			p, err := decodePoint(rs.Value)
			if err != nil {
				return Result{}, err
			}
			s.Points = []Point{p}
		} else {
			s.Points = make([]Point, 0, len(rs.Values))
			for _, v := range rs.Values {
				p, err := decodePoint(v)
				if err != nil {
					return Result{}, err
				}
				s.Points = append(s.Points, p)
			}
		}
		out.Series = append(out.Series, s)
	}
	return out, nil
}

func decodePoint(raw json.RawMessage) (Point, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return Point{}, fmt.Errorf("%w: a sample must be a [time, value] pair", ErrMalformedResponse)
	}
	var at float64
	if err := json.Unmarshal(pair[0], &at); err != nil || math.IsNaN(at) || math.IsInf(at, 0) {
		return Point{}, fmt.Errorf("%w: sample time is not a number", ErrMalformedResponse)
	}
	var text string
	if err := json.Unmarshal(pair[1], &text); err != nil {
		return Point{}, fmt.Errorf("%w: sample value is not a string", ErrMalformedResponse)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return Point{}, fmt.Errorf("%w: sample value %q is not a number", ErrMalformedResponse, excerpt([]byte(text)))
	}
	sec, frac := math.Modf(at)
	return Point{At: time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC(), Value: value}, nil
}

func excerpt(body []byte) string {
	text := strings.TrimSpace(string(bytes.ToValidUTF8(body, []byte("�"))))
	if len(text) <= maxExcerptBytes {
		return text
	}
	cut := maxExcerptBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
