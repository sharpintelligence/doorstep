package config

import (
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	DefaultBranch          = "main"
	DefaultArgoCDNamespace = "argocd"
	DefaultMaxBatch        = 10
	MaxMaxBatch            = 100
)

type Catalog struct {
	Apps          []App
	Notifications []Notification
	Auth          Auth
}

func (c Catalog) App(name string) (App, bool) {
	for _, a := range c.Apps {
		if a.Name == name {
			return a, true
		}
	}
	return App{}, false
}

func (c Catalog) Notification(name string) (Notification, bool) {
	for _, n := range c.Notifications {
		if n.Name == name {
			return n, true
		}
	}
	return Notification{}, false
}

func (c Catalog) SecretRefs() []SecretRef {
	var refs []SecretRef
	for _, n := range c.Notifications {
		if endpoint := n.Endpoint(); endpoint.Value == "" {
			refs = append(refs, endpoint)
		}
		if secret, ok := n.SigningSecret(); ok {
			refs = append(refs, secret)
		}
	}
	if secret, ok := c.Auth.ClientSecret(); ok {
		refs = append(refs, secret)
	}
	return refs
}

type RepoName string

func (r RepoName) Owner() string {
	owner, _, _ := strings.Cut(string(r), "/")
	return owner
}

func (r RepoName) Name() string {
	_, name, _ := strings.Cut(string(r), "/")
	return name
}

type App struct {
	Name   string   `yaml:"name"`
	Source Source   `yaml:"source"`
	Image  Image    `yaml:"image"`
	GitOps GitOps   `yaml:"gitops"`
	Pin    Pin      `yaml:"pin"`
	Deploy Deploy   `yaml:"deploy"`
	ArgoCD ArgoCD   `yaml:"argocd"`
	Gates  Gates    `yaml:"gates"`
	Notify []string `yaml:"notify"`
}

type Source struct {
	Repo           RepoName `yaml:"repo"`
	Branch         string   `yaml:"branch"`
	RequiredChecks []string `yaml:"required_checks"`
	Deployments    *bool    `yaml:"deployments"`
}

func (s Source) DeploymentsEnabled() bool {
	return s.Deployments == nil || *s.Deployments
}

type Image struct {
	Repository string `yaml:"repository"`
}

type GitOps struct {
	Repo   RepoName `yaml:"repo"`
	Branch string   `yaml:"branch"`
	Path   string   `yaml:"path"`
}

type PinStrategy string

const (
	PinKustomize PinStrategy = "kustomize"
	PinYAML      PinStrategy = "yaml"
)

type PinFormat string

const (
	PinFormatDigest    PinFormat = "digest"
	PinFormatReference PinFormat = "reference"
)

type Pin struct {
	Kustomize *KustomizePin `yaml:"kustomize"`
	YAML      *YAMLPin      `yaml:"yaml"`
}

type KustomizePin struct {
	Image string `yaml:"image"`
}

type YAMLPin struct {
	File   string    `yaml:"file"`
	Path   string    `yaml:"path"`
	Format PinFormat `yaml:"format"`
}

func (p Pin) Strategy() PinStrategy {
	switch {
	case p.Kustomize != nil && p.YAML == nil:
		return PinKustomize
	case p.YAML != nil && p.Kustomize == nil:
		return PinYAML
	default:
		return ""
	}
}

func (y YAMLPin) Segments() []string {
	if y.Path == "" {
		return nil
	}
	return strings.Split(y.Path, ".")
}

type DeployMode string

const (
	DeployCommit      DeployMode = "commit"
	DeployPullRequest DeployMode = "pull_request"
)

type Deploy struct {
	Mode     DeployMode `yaml:"mode"`
	MaxBatch *int       `yaml:"max_batch"`
}

func (d Deploy) BatchLimit() int {
	if d.MaxBatch == nil {
		return DefaultMaxBatch
	}
	return *d.MaxBatch
}

type ArgoCD struct {
	Application string    `yaml:"application"`
	Namespace   string    `yaml:"namespace"`
	SyncTimeout *Duration `yaml:"sync_timeout"`
}

func (a ArgoCD) Timeout() time.Duration {
	if a.SyncTimeout == nil {
		return core.DefaultSyncTimeout
	}
	return a.SyncTimeout.Std()
}

type GateType string

const (
	GateChecks   GateType = "checks"
	GateLock     GateType = "lock"
	GateSchedule GateType = "schedule"
	GateHealth   GateType = "health"
	GateSoak     GateType = "soak"
)

const DefaultSoakTimeoutAllowance = 10 * time.Minute

type GatePhase string

const (
	PhaseBefore GatePhase = "before"
	PhaseAfter  GatePhase = "after"
)

type Gates struct {
	Before []Gate `yaml:"before"`
	After  []Gate `yaml:"after"`
}

type Gate struct {
	Type             GateType  `yaml:"type"`
	Name             string    `yaml:"name"`
	Timeout          *Duration `yaml:"timeout"`
	Duration         Duration  `yaml:"duration"`
	Timezone         string    `yaml:"timezone"`
	ApplyToRollbacks bool      `yaml:"apply_to_rollbacks"`
	Windows          []Window  `yaml:"windows"`
}

type WindowEffect string

const (
	WindowAllow WindowEffect = "allow"
	WindowDeny  WindowEffect = "deny"
)

type Window struct {
	Effect   WindowEffect `yaml:"effect"`
	Cron     string       `yaml:"cron"`
	Duration Duration     `yaml:"duration"`
	Timezone string       `yaml:"timezone"`
	Name     string       `yaml:"name"`
}

type gateSpec struct {
	phase            GatePhase
	allowsTimeout    bool
	defaultTimeout   func(Gate) time.Duration
	requiresDuration bool
	requiresWindows  bool
}

func gateSpecs() map[GateType]gateSpec {
	return map[GateType]gateSpec{
		GateChecks:   {phase: PhaseBefore, allowsTimeout: true, defaultTimeout: fixedTimeout(30 * time.Minute)},
		GateLock:     {phase: PhaseBefore},
		GateSchedule: {phase: PhaseBefore, requiresWindows: true},
		GateHealth:   {phase: PhaseAfter, allowsTimeout: true, defaultTimeout: fixedTimeout(10 * time.Minute)},
		GateSoak:     {phase: PhaseAfter, allowsTimeout: true, defaultTimeout: soakTimeout, requiresDuration: true},
	}
}

func fixedTimeout(d time.Duration) func(Gate) time.Duration {
	return func(Gate) time.Duration { return d }
}

func soakTimeout(g Gate) time.Duration {
	return g.Duration.Std() + DefaultSoakTimeoutAllowance
}

type NotificationType string

const (
	NotifySlack   NotificationType = "slack"
	NotifyWebhook NotificationType = "webhook"
)

type Notification struct {
	Name           string           `yaml:"name"`
	Type           NotificationType `yaml:"type"`
	WebhookURLEnv  string           `yaml:"webhook_url_env"`
	WebhookURLFile string           `yaml:"webhook_url_file"`
	URL            string           `yaml:"url"`
	URLEnv         string           `yaml:"url_env"`
	URLFile        string           `yaml:"url_file"`
	SecretEnv      string           `yaml:"secret_env"`
	SecretFile     string           `yaml:"secret_file"`
	Events         []string         `yaml:"events"`
}

func (n Notification) EventNames() []string {
	if n.Events == nil {
		return DefaultNotificationEvents()
	}
	return slices.Clone(n.Events)
}

func (n Notification) Endpoint() SecretRef {
	if n.Type == NotifySlack {
		return SecretRef{Label: n.label("webhook_url"), Env: n.WebhookURLEnv, File: n.WebhookURLFile}
	}
	return SecretRef{Label: n.label("url"), Value: n.URL, Env: n.URLEnv, File: n.URLFile}
}

func (n Notification) SigningSecret() (SecretRef, bool) {
	ref := SecretRef{Label: n.label("secret"), Env: n.SecretEnv, File: n.SecretFile}
	return ref, ref.Env != "" || ref.File != ""
}

func (n Notification) label(field string) string {
	return "notification " + n.Name + " " + field
}

func (a App) withDefaults() App {
	a.Source.Branch = orDefault(a.Source.Branch, DefaultBranch)
	a.Source.RequiredChecks = slices.Clone(a.Source.RequiredChecks)
	if a.Source.Deployments != nil {
		a.Source.Deployments = new(*a.Source.Deployments)
	}
	a.GitOps.Branch = orDefault(a.GitOps.Branch, DefaultBranch)
	a.GitOps.Path = cleanRelative(a.GitOps.Path)
	a.Pin = a.Pin.withDefaults(a.Image.Repository)
	a.Deploy.Mode = orDefault(a.Deploy.Mode, DeployCommit)
	maxBatch := a.Deploy.BatchLimit()
	a.Deploy.MaxBatch = &maxBatch
	a.ArgoCD.Application = orDefault(a.ArgoCD.Application, a.Name)
	a.ArgoCD.Namespace = orDefault(a.ArgoCD.Namespace, DefaultArgoCDNamespace)
	a.ArgoCD.SyncTimeout = new(Duration(a.ArgoCD.Timeout()))
	a.Gates = a.Gates.withDefaults()
	a.Notify = slices.Clone(a.Notify)
	return a
}

func (p Pin) withDefaults(repository string) Pin {
	if p.Kustomize == nil && p.YAML == nil {
		return Pin{Kustomize: &KustomizePin{Image: repository}}
	}
	var out Pin
	if p.Kustomize != nil {
		k := *p.Kustomize
		k.Image = orDefault(k.Image, repository)
		out.Kustomize = &k
	}
	if p.YAML != nil {
		y := *p.YAML
		y.File = cleanRelative(y.File)
		y.Format = orDefault(y.Format, PinFormatDigest)
		out.YAML = &y
	}
	return out
}

func (g Gates) withDefaults() Gates {
	specs := gateSpecs()
	return Gates{
		Before: gatesWithDefaults(g.Before, []Gate{{Type: GateChecks}}, specs),
		After:  gatesWithDefaults(g.After, []Gate{{Type: GateHealth}}, specs),
	}
}

func gatesWithDefaults(gates, fallback []Gate, specs map[GateType]gateSpec) []Gate {
	if gates == nil {
		gates = fallback
	}
	out := make([]Gate, 0, len(gates))
	for _, g := range gates {
		g.Name = orDefault(g.Name, string(g.Type))
		spec, known := specs[g.Type]
		if known && spec.allowsTimeout && g.Timeout == nil {
			timeout := Duration(spec.defaultTimeout(g))
			g.Timeout = &timeout
		} else if g.Timeout != nil {
			timeout := *g.Timeout
			g.Timeout = &timeout
		}
		if known && spec.requiresWindows {
			g.Timezone = orDefault(g.Timezone, DefaultScheduleZone)
		}
		g.Windows = slices.Clone(g.Windows)
		for i := range g.Windows {
			g.Windows[i].Timezone = orDefault(g.Windows[i].Timezone, g.Timezone)
		}
		out = append(out, g)
	}
	return out
}

func orDefault[T ~string](v, fallback T) T {
	if v == "" {
		return fallback
	}
	return v
}

func cleanRelative(p string) string {
	if p == "" {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func (p Pin) target(gitops GitOps) (string, bool) {
	file := "kustomization"
	var key string
	switch p.Strategy() {
	case PinKustomize:
		key = "kustomize image " + p.Kustomize.Image
	case PinYAML:
		file = p.YAML.File
		key = "yaml path " + p.YAML.Path
	default:
		return "", false
	}
	return strings.Join([]string{
		strings.ToLower(string(gitops.Repo)),
		gitops.Branch,
		path.Join(gitops.Path, file),
		key,
	}, "\x00"), true
}

func (a App) kustomizationDir() (string, bool) {
	var dir string
	switch a.Pin.Strategy() {
	case PinKustomize:
		dir = path.Clean(a.GitOps.Path)
	case PinYAML:
		file := path.Join(a.GitOps.Path, a.Pin.YAML.File)
		if base := path.Base(file); base != "kustomization.yaml" && base != "kustomization.yml" {
			return "", false
		}
		dir = path.Dir(file)
	default:
		return "", false
	}
	return strings.Join([]string{strings.ToLower(string(a.GitOps.Repo)), a.GitOps.Branch, dir}, "\x00"), true
}
