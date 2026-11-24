package web

import (
	"encoding/json"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	shortSHALength    = 7
	shortDigestLength = len("sha256:") + 12
)

var evidenceLinkKeys = []string{"logs_url", "url"}

type link struct {
	URL  string
	Text string
}

func (h *Handler) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":         h.static.url,
		"shortSHA":      shortSHA,
		"shortDigest":   shortDigest,
		"relTime":       h.relTime,
		"isoTime":       isoTime,
		"stateClass":    stateClass,
		"gateClass":     gateClass,
		"verdictClass":  verdictClass,
		"isRetry":       isRetryEvent,
		"commitLink":    h.commitLink,
		"revisionLink":  h.revisionLink,
		"prettyJSON":    prettyJSON,
		"evidenceLinks": evidenceLinks,
		"csrfHeaders":   csrfHeaders,
		"pathEscape":    url.PathEscape,
	}
}

func shortSHA(sha string) string {
	return sha[:min(len(sha), shortSHALength)]
}

func shortDigest(d core.Digest) string {
	return string(d)[:min(len(d), shortDigestLength)]
}

func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, !t.IsZero()
	default:
		return time.Time{}, false
	}
}

func isoTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (h *Handler) relTime(v any) string {
	t, ok := asTime(v)
	if !ok {
		return ""
	}
	d := h.now().Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var amount string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		amount = plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		amount = plural(int(d/time.Hour), "hour")
	default:
		amount = plural(int(d/(24*time.Hour)), "day")
	}
	if future {
		return "in " + amount
	}
	return amount + " ago"
}

func plural(n int, unit string) string {
	s := strconv.Itoa(n) + " " + unit
	if n != 1 {
		s += "s"
	}
	return s
}

func stateClass(s core.State) string {
	switch {
	case s == core.StateSucceeded:
		return "badge ok"
	case s == core.StateFailed:
		return "badge bad"
	case s.Terminal():
		return "badge muted"
	default:
		return "badge active"
	}
}

func gateClass(s core.GateStatus) string {
	switch s {
	case core.GatePassed:
		return "badge ok"
	case core.GateFailed:
		return "badge bad"
	case core.GateSkipped:
		return "badge muted"
	case core.GatePending:
		return "badge active"
	default:
		return "badge muted"
	}
}

func (h *Handler) commitLink(repository, sha string) link {
	l := link{Text: shortSHA(sha)}
	if h.commitURL != nil && strings.Count(repository, "/") == 1 && core.ValidateCommitSHA(sha) == nil {
		l.URL = h.commitURL(repository, sha)
	}
	return l
}

func (h *Handler) revisionLink(repository, revision string) link {
	if core.ValidateCommitSHA(revision) == nil {
		return h.commitLink(repository, revision)
	}
	return link{Text: revision}
}

func evidenceLinks(raw json.RawMessage) []link {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var links []link
	for _, key := range evidenceLinkKeys {
		value, ok := fields[key].(string)
		if !ok {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			continue
		}
		links = append(links, link{URL: u.String(), Text: key})
	}
	return links
}

func csrfHeaders(token string) (string, error) {
	data, err := json.Marshal(map[string]string{auth.CSRFHeader: token})
	return string(data), err
}
