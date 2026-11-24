package notify

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestSlackMessageGolden(t *testing.T) {
	t.Parallel()

	for _, s := range samples() {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			body, err := encodeJSON(slackMessageFor(s.n, testLinks()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertGolden(t, "slack/"+s.name+".json", body)
		})
	}
}

func TestSlackMessageWithoutLinks(t *testing.T) {
	t.Parallel()

	msg := slackMessageFor(sampleNamed(t, "deploy-rolled-back"), Links{})
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "http") {
		t.Errorf("message without links contains a URL: %s", body)
	}
	for _, b := range msg.Attachments[0].Blocks {
		if b.Type == "context" {
			t.Errorf("message without a public URL has a context block: %+v", b)
		}
	}
}

func TestSlackEscapesDynamicText(t *testing.T) {
	t.Parallel()

	n := sampleNamed(t, "deploy-failed")
	n.Deployment.RequestedBy = "<!channel> & <@U123>"
	n.Deployment.FailureReason = "<https://evil.example.com|click> " + strings.Repeat("x", 600)
	n.Message = "<!here> deploy > all"
	n.Target.Environment = "<prod>"
	n.Deployment.Target = n.Target
	n.Deployment.Commit.Repository = "acme/<b>"

	body, err := encodeJSON(slackMessageFor(n, testLinks()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded slackMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var texts []string
	texts = append(texts, decoded.Text)
	for _, b := range decoded.Attachments[0].Blocks {
		if b.Text != nil {
			texts = append(texts, b.Text.Text)
		}
		for _, f := range append(b.Fields, b.Elements...) {
			texts = append(texts, f.Text)
		}
	}
	all := strings.Join(texts, "\n")
	for _, forbidden := range []string{"<!channel>", "<@U123>", "<!here>", "<https://evil", "<prod>", "<b>"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("message contains unescaped %q:\n%s", forbidden, all)
		}
	}
	for _, want := range []string{"&lt;!channel&gt; &amp; &lt;@U123&gt;", "&lt;!here&gt; deploy &gt; all", "(&lt;prod&gt;)"} {
		if !strings.Contains(all, want) {
			t.Errorf("message does not contain %q:\n%s", want, all)
		}
	}
	for _, f := range decoded.Attachments[0].Blocks[1].Fields {
		if strings.HasPrefix(f.Text, "*Reason*") && len([]rune(f.Text)) > len("*Reason*\n")+600 {
			t.Errorf("reason was not truncated: %d characters", len([]rune(f.Text)))
		}
	}
}

func TestSlackLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url, text, want string
	}{
		{url: "https://x.example/a?b=1&c=2", text: "View", want: "<https://x.example/a?b=1&amp;c=2|View>"},
		{url: "https://x.example", text: "a|b<c>", want: "<https://x.example|a¦b&lt;c&gt;>"},
	}
	for _, tt := range tests {
		if got := slackLink(tt.url, tt.text); got != tt.want {
			t.Errorf("slackLink(%q, %q) = %q, want %q", tt.url, tt.text, got, tt.want)
		}
	}
}

func TestSlackSend(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotHeader = body, r.Header.Clone()
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var reads atomic.Int32
	sink, err := NewSlack(SlackOptions{
		Name:      "ops",
		URL:       func() (string, error) { reads.Add(1); return srv.URL + secretPath, nil },
		Links:     testLinks(),
		UserAgent: "doorstep/test",
	})
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}
	if sink.Name() != "ops" || sink.Type() != TypeSlack {
		t.Errorf("Name, Type = %q, %q", sink.Name(), sink.Type())
	}
	n := sampleNamed(t, "deploy-succeeded")
	delivery := sink.Prepare(n)
	for range 2 {
		if err := delivery(t.Context()); err != nil {
			t.Fatalf("delivery: %v", err)
		}
	}
	if reads.Load() != 2 {
		t.Errorf("webhook URL read %d times, want once per attempt", reads.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get("User-Agent") != "doorstep/test" {
		t.Errorf("headers = %v", gotHeader)
	}
	want, _ := encodeJSON(slackMessageFor(n, testLinks()))
	if string(gotBody) != string(want) {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestSlackAccepts(t *testing.T) {
	t.Parallel()

	url := func() (string, error) { return "https://hooks.slack.com/services/x", nil }
	defaults, err := NewSlack(SlackOptions{Name: "ops", URL: url})
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}
	failures, err := NewSlack(SlackOptions{Name: "ops", URL: url, Kinds: []core.NotificationKind{core.NotificationDeployFailed}})
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}
	for _, s := range samples() {
		wantDefault := s.n.Kind != core.NotificationDeployQueued && s.n.Kind != core.NotificationDeployCancelled && s.n.Kind != core.NotificationDeploySuperseded
		if got := defaults.Accepts(s.n); got != wantDefault {
			t.Errorf("default Accepts(%s) = %v, want %v", s.name, got, wantDefault)
		}
		if got := failures.Accepts(s.n); got != (s.n.Kind == core.NotificationDeployFailed) {
			t.Errorf("failures-only Accepts(%s) = %v", s.name, got)
		}
	}
}

func TestNewSlackErrors(t *testing.T) {
	t.Parallel()

	url := func() (string, error) { return "", nil }
	tests := []struct {
		name string
		opts SlackOptions
	}{
		{name: "no name", opts: SlackOptions{URL: url}},
		{name: "no url", opts: SlackOptions{Name: "ops"}},
		{name: "empty kinds", opts: SlackOptions{Name: "ops", URL: url, Kinds: []core.NotificationKind{}}},
		{name: "unknown kind", opts: SlackOptions{Name: "ops", URL: url, Kinds: []core.NotificationKind{"deploy.exploded"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewSlack(tt.opts); err == nil {
				t.Error("NewSlack() = nil error")
			}
		})
	}
}

func TestSlackURLNeverLeaks(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no_service")
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		url  func() (string, error)
		want string
	}{
		{name: "404", url: func() (string, error) { return srv.URL + secretPath, nil }, want: "404"},
		{name: "unreachable", url: func() (string, error) { return "http://127.0.0.1:1" + secretPath, nil }, want: "post request"},
		{name: "not a URL", url: func() (string, error) { return "s3cr3t-t0k3n", nil }, want: "endpoint must be"},
		{name: "secret unreadable", url: func() (string, error) { return "", errors.New("notification ops webhook_url: file /x does not exist") }, want: "read slack webhook URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := newTestLogger()
			observer := &recordingObserver{}
			sink, err := NewSlack(SlackOptions{Name: "ops", URL: tt.url})
			if err != nil {
				t.Fatalf("NewSlack: %v", err)
			}
			d := newTestDispatcher(t, Options{Sinks: []Sink{sink}, Routes: map[string][]string{"blog": {"ops"}}, Logger: logger, Observer: observer})
			d.Notify(t.Context(), sampleNamed(t, "deploy-failed"))
			closeDispatcher(t, d)
			if observer.count(TypeSlack, ResultFailed) != 1 {
				t.Errorf("observations = %v, want one slack failure", observer.seen)
			}
			out := logs.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("logs do not contain %q: %s", tt.want, out)
			}
			if strings.Contains(out, "s3cr3t") || strings.Contains(out, "/services/") {
				t.Errorf("logs leak the webhook URL: %s", out)
			}
		})
	}
}
