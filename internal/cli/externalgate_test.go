package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

type noReports struct{}

func (noReports) GetGateReport(context.Context, core.DeploymentID, string) (core.GateReport, error) {
	return core.GateReport{}, core.ErrNotFound
}

func TestRegisterExternalGate(t *testing.T) {
	t.Parallel()

	r := gates.NewRegistry()
	if err := registerExternalGate(r, noReports{}); err != nil {
		t.Fatalf("registerExternalGate: %v", err)
	}
	app := config.App{Name: "blog", Gates: config.Gates{Before: []config.Gate{{Type: config.GateExternal, Name: "approval"}}}}
	plan, err := r.Plan(app)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Before) != 1 || plan.Before[0].Kind != metrics.GateKindExternal {
		t.Errorf("plan = %+v", plan)
	}
	if err := registerExternalGate(r, noReports{}); err == nil {
		t.Error("registering twice succeeded")
	}
}

func TestNewGateStatusMirrorWithoutGitHub(t *testing.T) {
	t.Parallel()

	mirror, err := newGateStatusMirror(config.Catalog{}, config.Default(), nil, nil, nil)
	if err != nil || mirror != nil {
		t.Errorf("newGateStatusMirror = %v, %v; want nil, nil without a GitHub App", mirror, err)
	}
}

func TestNewGateStatusMirror(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	srv.Install("example-user", "blog", 1)
	srv.Install("example-user", "api", 1)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	catalog := config.File{Apps: []config.App{
		{Name: "blog"},
		{Name: "api", Source: config.Source{Statuses: new(false)}},
	}}.Catalog()
	cfg := config.Default()
	cfg.GitHub.APIURL = srv.URL
	cfg.Server.PublicURL = "https://doorstep.example.com"
	m := metrics.New(metrics.Options{})

	mirror, err := newGateStatusMirror(catalog, cfg, app, nil, m.Recorder())
	if err != nil || mirror == nil {
		t.Fatalf("newGateStatusMirror = %v, %v", mirror, err)
	}
	var observer gates.Observer = mirror
	for _, name := range []string{"blog", "api"} {
		d := *deployNotification(name, core.NotificationDeployStarted, 3).Deployment
		result := core.GateResult{DeploymentID: 3, Name: "e2e", Type: "external", Phase: core.GatePhaseAfter, Status: core.GatePassed, Message: "green"}
		if err := observer.GateRecorded(t.Context(), d, result); err != nil {
			t.Fatalf("GateRecorded: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := mirror.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	posted := srv.PostedStatuses("example-user", "blog", notifyTestSHA)
	want := githubtest.Status{Context: "doorstep/blog/after/e2e", State: "success", Description: "green", TargetURL: "https://doorstep.example.com/deployments/3"}
	if len(posted) != 1 || posted[0] != want {
		t.Errorf("blog statuses = %+v, want %+v", posted, want)
	}
	if posted := srv.PostedStatuses("example-user", "api", notifyTestSHA); len(posted) != 0 {
		t.Errorf("api opted out but got %+v", posted)
	}
	wantMetrics := `
# HELP doorstep_notifications_total Notification deliveries, by target type and result.
# TYPE doorstep_notifications_total counter
doorstep_notifications_total{result="dropped",type="github"} 0
doorstep_notifications_total{result="dropped",type="slack"} 0
doorstep_notifications_total{result="dropped",type="webhook"} 0
doorstep_notifications_total{result="failed",type="github"} 0
doorstep_notifications_total{result="failed",type="slack"} 0
doorstep_notifications_total{result="failed",type="webhook"} 0
doorstep_notifications_total{result="sent",type="github"} 1
doorstep_notifications_total{result="sent",type="slack"} 0
doorstep_notifications_total{result="sent",type="webhook"} 0
`
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(wantMetrics), "doorstep_notifications_total"); err != nil {
		t.Error(err)
	}
}

func TestNewGateStatusMirrorInvalidAPIURL(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	cfg := config.Default()
	cfg.GitHub.APIURL = "ftp://github.example.com"
	if mirror, err := newGateStatusMirror(config.Catalog{}, cfg, app, nil, nil); err == nil {
		_ = mirror.Close(t.Context())
		t.Error("invalid GitHub API URL accepted")
	}
}
