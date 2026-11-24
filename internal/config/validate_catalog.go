package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

func NotificationEvents() []string {
	return []string{
		"deploy.queued",
		"deploy.started",
		"deploy.succeeded",
		"deploy.failed",
		"deploy.rolled_back",
		"deploy.cancelled",
		"deploy.superseded",
		"lock.changed",
		"lock.reminder",
	}
}

func DefaultNotificationEvents() []string {
	return []string{
		"deploy.started",
		"deploy.succeeded",
		"deploy.failed",
		"deploy.rolled_back",
		"lock.changed",
		"lock.reminder",
	}
}

type reporter struct {
	prefix string
	errs   *[]error
}

func newReporter(errs *[]error, list string, index int, name string) reporter {
	prefix := fmt.Sprintf("%s[%d]", list, index)
	if name != "" {
		prefix += " (" + name + ")"
	}
	return reporter{prefix: prefix, errs: errs}
}

func (r reporter) check(field string, err error) {
	if err != nil {
		*r.errs = append(*r.errs, fmt.Errorf("%s: %s: %w", r.prefix, field, err))
	}
}

func (r reporter) addf(field, format string, args ...any) {
	r.check(field, fmt.Errorf(format, args...))
}

func (c Catalog) Validate() error {
	var errs []error

	notifications := make(map[string]bool, len(c.Notifications))
	firstNotification := make(map[string]int, len(c.Notifications))
	for i, n := range c.Notifications {
		r := newReporter(&errs, "notifications", i, n.Name)
		if first, dup := firstNotification[n.Name]; dup && n.Name != "" {
			r.addf("name", "duplicate of notifications[%d]", first)
		} else {
			firstNotification[n.Name] = i
		}
		notifications[n.Name] = true
		validateNotification(r, n)
	}

	kustomizePins := make(map[string]int, len(c.Apps))
	for i, a := range c.Apps {
		if dir, ok := a.kustomizationDir(); ok && a.Pin.Strategy() == PinKustomize {
			if _, seen := kustomizePins[dir]; !seen {
				kustomizePins[dir] = i
			}
		}
	}

	firstApp := make(map[string]int, len(c.Apps))
	pinOwners := make(map[string]int, len(c.Apps))
	for i, a := range c.Apps {
		r := newReporter(&errs, "apps", i, a.Name)
		if first, dup := firstApp[a.Name]; dup && a.Name != "" {
			r.addf("name", "duplicate of apps[%d]", first)
		} else {
			firstApp[a.Name] = i
		}
		validateApp(r, a, notifications)
		if target, ok := a.Pin.target(a.GitOps); ok {
			if first, dup := pinOwners[target]; dup {
				other := c.Apps[first]
				r.addf("pin", "writes the same image pin as apps[%d] (%s) in %s on branch %s", first, other.Name, a.GitOps.Repo, a.GitOps.Branch)
			} else {
				pinOwners[target] = i
			}
		}
		if dir, ok := a.kustomizationDir(); ok && a.Pin.Strategy() == PinYAML {
			if owner, found := kustomizePins[dir]; found {
				r.addf("pin.yaml.file", "writes the kustomization that apps[%d] (%s) pins with kustomize; use pin.kustomize instead", owner, c.Apps[owner].Name)
			}
		}
	}

	errs = append(errs, c.Auth.validate()...)
	return errors.Join(errs...)
}

type field struct {
	name  string
	value string
}

func validateNotification(r reporter, n Notification) {
	r.check("name", validateName(n.Name))
	switch n.Type {
	case NotifySlack:
		exactlyOne(r, []field{{"webhook_url_env", n.WebhookURLEnv}, {"webhook_url_file", n.WebhookURLFile}})
		forbidFields(r, n.Type, []field{
			{"url", n.URL},
			{"url_env", n.URLEnv},
			{"url_file", n.URLFile},
			{"secret_env", n.SecretEnv},
			{"secret_file", n.SecretFile},
		})
	case NotifyWebhook:
		exactlyOne(r, []field{{"url", n.URL}, {"url_env", n.URLEnv}, {"url_file", n.URLFile}})
		if n.SecretEnv != "" && n.SecretFile != "" {
			r.addf("secret_env, secret_file", "set at most one")
		}
		forbidFields(r, n.Type, []field{{"webhook_url_env", n.WebhookURLEnv}, {"webhook_url_file", n.WebhookURLFile}})
		if n.URL != "" {
			r.check("url", validateURL(n.URL))
		}
	case "":
		r.addf("type", "is required (want slack or webhook)")
	default:
		r.addf("type", "unknown type %q (want slack or webhook)", n.Type)
	}
	validateEvents(r, n.Events)
	for _, f := range []field{{"webhook_url_env", n.WebhookURLEnv}, {"url_env", n.URLEnv}, {"secret_env", n.SecretEnv}} {
		if f.value != "" {
			r.check(f.name, validateEnvName(f.value))
		}
	}
	for _, f := range []field{{"webhook_url_file", n.WebhookURLFile}, {"url_file", n.URLFile}, {"secret_file", n.SecretFile}} {
		if f.value != "" {
			r.check(f.name, validateSecretFile(f.value))
		}
	}
}

func validateEvents(r reporter, events []string) {
	if events == nil {
		return
	}
	if len(events) == 0 {
		r.addf("events", "must not be empty; leave it out for the default events")
		return
	}
	known := NotificationEvents()
	seen := make(map[string]bool, len(events))
	for i, event := range events {
		field := fmt.Sprintf("events[%d]", i)
		switch {
		case !slices.Contains(known, event):
			r.addf(field, "unknown event %q (want one of %s)", event, strings.Join(known, ", "))
		case seen[event]:
			r.addf(field, "duplicate event %q", event)
		}
		seen[event] = true
	}
}

func exactlyOne(r reporter, fields []field) {
	names := make([]string, 0, len(fields))
	set := 0
	for _, f := range fields {
		names = append(names, f.name)
		if f.value != "" {
			set++
		}
	}
	if set != 1 {
		r.addf(strings.Join(names, ", "), "set exactly one")
	}
}

func forbidFields(r reporter, t NotificationType, fields []field) {
	for _, f := range fields {
		if f.value != "" {
			r.addf(f.name, "not allowed for type %s", t)
		}
	}
}

func validateApp(r reporter, a App, notifications map[string]bool) {
	r.check("name", validateName(a.Name))

	r.check("source.repo", validateRepo(a.Source.Repo))
	r.check("source.branch", validateBranch(a.Source.Branch))
	checks := make(map[string]bool, len(a.Source.RequiredChecks))
	for i, check := range a.Source.RequiredChecks {
		field := fmt.Sprintf("source.required_checks[%d]", i)
		switch {
		case strings.TrimSpace(check) == "":
			r.addf(field, "must not be empty")
		case checks[check]:
			r.addf(field, "duplicate check %q", check)
		}
		checks[check] = true
	}

	r.check("image.repository", validateImageRepository(a.Image.Repository))

	r.check("gitops.repo", validateRepo(a.GitOps.Repo))
	r.check("gitops.branch", validateBranch(a.GitOps.Branch))
	r.check("gitops.path", validateRelativePath(a.GitOps.Path))

	validatePin(r, a.Pin)

	switch a.Deploy.Mode {
	case DeployCommit, DeployPullRequest:
	default:
		r.addf("deploy.mode", "unknown mode %q (want commit or pull_request)", a.Deploy.Mode)
	}
	if n := a.Deploy.BatchLimit(); n < 1 || n > MaxMaxBatch {
		r.addf("deploy.max_batch", "%d is out of range (want 1 to %d)", n, MaxMaxBatch)
	}

	r.check("argocd.application", validateDNSSubdomain(a.ArgoCD.Application))
	if a.ArgoCD.Namespace == "" {
		r.addf("argocd.namespace", "is required")
	} else if !isDNSLabel(a.ArgoCD.Namespace) {
		r.addf("argocd.namespace", "%q must be a DNS label", a.ArgoCD.Namespace)
	}
	if a.ArgoCD.SyncTimeout != nil && *a.ArgoCD.SyncTimeout <= 0 {
		r.addf("argocd.sync_timeout", "must be positive")
	}

	validateGates(r, a.Gates)

	seen := make(map[string]bool, len(a.Notify))
	for i, name := range a.Notify {
		field := fmt.Sprintf("notify[%d]", i)
		switch {
		case !notifications[name]:
			r.addf(field, "unknown notification %q", name)
		case seen[name]:
			r.addf(field, "duplicate notification %q", name)
		}
		seen[name] = true
	}
}

func validatePin(r reporter, p Pin) {
	switch p.Strategy() {
	case PinKustomize:
		r.check("pin.kustomize.image", validateImageName(p.Kustomize.Image))
	case PinYAML:
		y := p.YAML
		switch {
		case y.File == "":
			r.addf("pin.yaml.file", "is required")
		case !strings.HasSuffix(y.File, ".yaml") && !strings.HasSuffix(y.File, ".yml"):
			r.addf("pin.yaml.file", "%q must end in .yaml or .yml", y.File)
		default:
			r.check("pin.yaml.file", validateRelativePath(y.File))
		}
		if y.Path == "" {
			r.addf("pin.yaml.path", "is required")
		} else if slices.ContainsFunc(y.Segments(), isBlankOrPadded) {
			r.addf("pin.yaml.path", "%q must be dot-separated keys without empty segments or surrounding whitespace", y.Path)
		}
		switch y.Format {
		case PinFormatDigest, PinFormatReference:
		default:
			r.addf("pin.yaml.format", "unknown format %q (want digest or reference)", y.Format)
		}
	default:
		r.addf("pin", "set exactly one of kustomize or yaml")
	}
}

func isBlankOrPadded(segment string) bool {
	return segment == "" || strings.TrimSpace(segment) != segment
}

func validateGates(r reporter, g Gates) {
	specs := gateSpecs()
	known := slices.Sorted(maps.Keys(specs))
	names := make(map[string]string)
	for _, phase := range []struct {
		phase GatePhase
		gates []Gate
	}{{PhaseBefore, g.Before}, {PhaseAfter, g.After}} {
		for i, gate := range phase.gates {
			field := fmt.Sprintf("gates.%s[%d]", phase.phase, i)
			if err := validateName(gate.Name); err != nil {
				r.check(field+".name", err)
			} else if other, dup := names[gate.Name]; dup {
				r.addf(field+".name", "duplicate of %s", other)
			} else {
				names[gate.Name] = field
			}

			spec, ok := specs[gate.Type]
			if !ok {
				if gate.Type == "" {
					r.addf(field+".type", "is required (want %s)", joinTypes(known))
				} else {
					r.addf(field+".type", "unknown type %q (want %s)", gate.Type, joinTypes(known))
				}
				continue
			}
			if spec.phase != phase.phase {
				r.addf(field+".type", "%s gates run only in gates.%s", gate.Type, spec.phase)
			}
			if spec.allowsTimeout && (gate.Timeout == nil || *gate.Timeout <= 0) {
				r.addf(field+".timeout", "must be positive")
			}
			if !spec.allowsTimeout && gate.Timeout != nil {
				r.addf(field+".timeout", "not allowed for type %s", gate.Type)
			}
			if spec.requiresDuration && gate.Duration <= 0 {
				r.addf(field+".duration", "is required for type %s and must be positive", gate.Type)
			}
			if !spec.requiresDuration && gate.Duration != 0 {
				r.addf(field+".duration", "not allowed for type %s", gate.Type)
			}
			if spec.requiresDuration && spec.allowsTimeout && gate.Duration > 0 && gate.Timeout != nil && *gate.Timeout > 0 && *gate.Timeout <= gate.Duration {
				r.addf(field+".timeout", "%s must be longer than duration %s", gate.Timeout, gate.Duration)
			}
			if spec.requiresWindows {
				validateScheduleGate(r, field, gate)
			} else {
				forbidScheduleFields(r, field, gate)
			}
		}
	}
}

func joinTypes(types []GateType) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}
