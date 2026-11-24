package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

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
	}

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

	r.check("argocd.application", validateDNSSubdomain(a.ArgoCD.Application))
	if a.ArgoCD.Namespace == "" {
		r.addf("argocd.namespace", "is required")
	} else if !isDNSLabel(a.ArgoCD.Namespace) {
		r.addf("argocd.namespace", "%q must be a DNS label", a.ArgoCD.Namespace)
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
		} else if slices.Contains(y.Segments(), "") {
			r.addf("pin.yaml.path", "%q must be dot-separated keys without empty segments", y.Path)
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
			if spec.allowsTimeout && gate.Timeout <= 0 {
				r.addf(field+".timeout", "must be positive")
			}
			if !spec.allowsTimeout && gate.Timeout != 0 {
				r.addf(field+".timeout", "not allowed for type %s", gate.Type)
			}
			if spec.requiresDuration && gate.Duration <= 0 {
				r.addf(field+".duration", "is required for type %s and must be positive", gate.Type)
			}
			if !spec.requiresDuration && gate.Duration != 0 {
				r.addf(field+".duration", "not allowed for type %s", gate.Type)
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
