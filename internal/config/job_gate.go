package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

type JobGate struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
	LogsURL   string `yaml:"logs_url"`
}

func JobLogsPlaceholders() []string {
	return []string{"app", "namespace", "job", "uid", "deployment", "argocd_namespace", "argocd_application"}
}

func (j JobGate) RenderLogsURL(values map[string]string) string {
	if j.LogsURL == "" {
		return ""
	}
	rendered, err := expandLogsURL(j.LogsURL, values)
	if err != nil {
		return ""
	}
	return rendered
}

func expandLogsURL(template string, values map[string]string) (string, error) {
	known := JobLogsPlaceholders()
	var b strings.Builder
	rest := template
	for {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		if rest[i] == '}' {
			return "", errors.New("has a '}' without a matching '{'; write a literal brace as %7D")
		}
		b.WriteString(rest[:i])
		end := strings.IndexByte(rest[i:], '}')
		if end < 0 {
			return "", errors.New("has a '{' without a matching '}'; write a literal brace as %7B")
		}
		key := rest[i+1 : i+end]
		if !slices.Contains(known, key) {
			return "", fmt.Errorf("unknown placeholder {%s} (want one of {%s})", key, strings.Join(known, "}, {"))
		}
		b.WriteString(url.PathEscape(values[key]))
		rest = rest[i+end+1:]
	}
}

func validateLogsURL(template string) error {
	if strings.ContainsFunc(template, unicode.IsSpace) {
		return errors.New("must not contain whitespace")
	}
	samples := make(map[string]string)
	for _, key := range JobLogsPlaceholders() {
		samples[key] = "sample"
	}
	rendered, err := expandLogsURL(template, samples)
	if err != nil {
		return err
	}
	u, err := url.Parse(rendered)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must be an absolute http or https URL")
	case u.Host == "":
		return errors.New("must include a host")
	case u.User != nil:
		return errors.New("must not include credentials")
	}
	return nil
}

func validateJobGate(r reporter, field string, g Gate) {
	if g.Job == nil {
		r.addf(field+".job", "is required for type %s", g.Type)
		return
	}
	switch ns := g.Job.Namespace; {
	case ns == "":
		r.addf(field+".job.namespace", "is required")
	case !isDNSLabel(ns):
		r.addf(field+".job.namespace", "%q must be a DNS label", ns)
	}
	r.check(field+".job.name", validateDNSSubdomain(g.Job.Name))
	if g.Job.LogsURL != "" {
		r.check(field+".job.logs_url", validateLogsURL(g.Job.LogsURL))
	}
}

func forbidJobFields(r reporter, field string, g Gate) {
	if g.Job != nil {
		r.addf(field+".job", "not allowed for type %s", g.Type)
	}
}
