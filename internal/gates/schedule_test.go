package gates_test

import (
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

var (
	weekendFreeze = config.Window{Effect: config.WindowDeny, Cron: "0 17 * * 5", Duration: config.Duration(63 * time.Hour), Name: "weekend"}
	businessHours = config.Window{Effect: config.WindowAllow, Cron: "0 9 * * 1-5", Duration: config.Duration(8 * time.Hour), Name: "business hours"}
	mondayRelease = config.Window{Effect: config.WindowDeny, Cron: "0 12 * * 1", Duration: config.Duration(2 * time.Hour)}
	eveningWindow = config.Window{Effect: config.WindowAllow, Cron: "0 20 * * *", Duration: config.Duration(time.Hour)}

	tuesdayMorningDeny     = config.Window{Effect: config.WindowDeny, Cron: "0 8 * * 2", Duration: config.Duration(2 * time.Hour)}
	tuesdayLateMorningDeny = config.Window{Effect: config.WindowDeny, Cron: "0 10 * * 2", Duration: config.Duration(time.Hour)}
	tuesdayDeny            = config.Window{Effect: config.WindowDeny, Cron: "0 0 * * 2", Duration: config.Duration(24 * time.Hour)}
)

func scheduleGate(timezone string, applyToRollbacks bool, windows ...config.Window) config.Gate {
	return config.Gate{Type: config.GateSchedule, Name: "schedule", Timezone: timezone, ApplyToRollbacks: applyToRollbacks, Windows: windows}
}

func TestScheduleGate(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name          string
		gate          config.Gate
		now           time.Time
		kind          core.Kind
		wantStatus    core.GateStatus
		wantMessage   string
		wantActive    int
		wantNextAllow string
	}{
		{
			name:        "weekend freeze holds on Saturday",
			gate:        scheduleGate("UTC", false, weekendFreeze),
			now:         time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC),
			wantStatus:  core.GatePending,
			wantMessage: "deploys are frozen until 2026-11-30T08:00:00Z (weekend)",
			wantActive:  1,
		},
		{
			name:        "weekend freeze inactive on Monday",
			gate:        scheduleGate("UTC", false, weekendFreeze),
			now:         monday,
			wantStatus:  core.GatePassed,
			wantMessage: "no deny window is active",
		},
		{
			name:        "inside business hours",
			gate:        scheduleGate("UTC", false, businessHours),
			now:         monday,
			wantStatus:  core.GatePassed,
			wantMessage: "inside deploy window until 2026-11-23T17:00:00Z (business hours)",
			wantActive:  1,
		},
		{
			name:          "after business hours",
			gate:          scheduleGate("UTC", false, businessHours),
			now:           time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-24T09:00:00Z",
			wantNextAllow: "2026-11-24T09:00:00Z",
		},
		{
			name:          "next opening is the nearest allow window",
			gate:          scheduleGate("UTC", false, businessHours, eveningWindow),
			now:           time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-23T20:00:00Z",
			wantNextAllow: "2026-11-23T20:00:00Z",
		},
		{
			name:          "next opening waits for a deny window covering the allow start",
			gate:          scheduleGate("UTC", false, businessHours, tuesdayMorningDeny),
			now:           time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-24T10:00:00Z",
			wantNextAllow: "2026-11-24T10:00:00Z",
		},
		{
			name:          "next opening waits for consecutive deny windows",
			gate:          scheduleGate("UTC", false, businessHours, tuesdayMorningDeny, tuesdayLateMorningDeny),
			now:           time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-24T11:00:00Z",
			wantNextAllow: "2026-11-24T11:00:00Z",
		},
		{
			name:          "next opening skips an allow window denied throughout",
			gate:          scheduleGate("UTC", false, businessHours, tuesdayDeny),
			now:           time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-25T09:00:00Z",
			wantNextAllow: "2026-11-25T09:00:00Z",
		},
		{
			name:        "no next opening when deny windows cover every allow window",
			gate:        scheduleGate("UTC", false, businessHours, config.Window{Effect: config.WindowDeny, Cron: "0 0 * * 1-5", Duration: config.Duration(24 * time.Hour)}),
			now:         time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
			wantStatus:  core.GatePending,
			wantMessage: "outside deploy windows",
		},
		{
			name:        "deny wins over an overlapping allow",
			gate:        scheduleGate("UTC", false, businessHours, mondayRelease),
			now:         monday,
			wantStatus:  core.GatePending,
			wantMessage: "deploys are frozen until 2026-11-23T14:00:00Z (0 12 * * 1)",
			wantActive:  2,
		},
		{
			name:        "allow after the deny ends",
			gate:        scheduleGate("UTC", false, businessHours, mondayRelease),
			now:         time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC),
			wantStatus:  core.GatePassed,
			wantMessage: "inside deploy window until 2026-11-23T17:00:00Z (business hours)",
			wantActive:  1,
		},
		{
			name:          "gate time zone applies to windows",
			gate:          scheduleGate("America/New_York", false, businessHours),
			now:           monday.Add(-6 * time.Hour),
			wantStatus:    core.GatePending,
			wantMessage:   "outside deploy windows; next opens 2026-11-23T14:00:00Z",
			wantNextAllow: "2026-11-23T14:00:00Z",
		},
		{
			name:        "rollbacks bypass the schedule by default",
			gate:        scheduleGate("UTC", false, weekendFreeze),
			now:         time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC),
			kind:        core.KindRollback,
			wantStatus:  core.GateSkipped,
			wantMessage: "skipped for rollback",
		},
		{
			name:        "rollbacks held when the schedule applies to them",
			gate:        scheduleGate("UTC", true, weekendFreeze),
			now:         time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC),
			kind:        core.KindRollback,
			wantStatus:  core.GatePending,
			wantMessage: "deploys are frozen until 2026-11-30T08:00:00Z (weekend)",
			wantActive:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			f.clock.now = tt.now
			gate := tt.gate
			for i := range gate.Windows {
				if gate.Windows[i].Timezone == "" {
					gate.Windows[i].Timezone = gate.Timezone
				}
			}
			g := builtinGate(t, f.deps(), webApp(), gate)
			kind := tt.kind
			if kind == "" {
				kind = core.KindDeploy
			}
			ev := evaluateGate(t, g, deployment(kind), nil)
			if ev.Status != tt.wantStatus || ev.Message != tt.wantMessage {
				t.Errorf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			if tt.wantStatus == core.GateSkipped {
				if len(ev.Evidence) != 0 {
					t.Errorf("skipped evidence = %s, want none", ev.Evidence)
				}
				return
			}
			evidence := evidenceMap(t, ev)
			if evidence["evaluated_at"] != tt.now.UTC().Format(time.RFC3339) {
				t.Errorf("evaluated_at = %v, want %s", evidence["evaluated_at"], tt.now)
			}
			if active, _ := evidence["active"].([]any); len(active) != tt.wantActive {
				t.Errorf("active windows = %v, want %d", evidence["active"], tt.wantActive)
			}
			if got, _ := evidence["next_allow"].(string); got != tt.wantNextAllow {
				t.Errorf("next_allow = %q, want %q", got, tt.wantNextAllow)
			}
		})
	}
}

func TestScheduleGateActiveEvidence(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.clock.now = time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	w := weekendFreeze
	w.Timezone = "Europe/London"
	g := builtinGate(t, f.deps(), webApp(), scheduleGate("Europe/London", false, w))
	ev := evaluateGate(t, g, deployment(core.KindDeploy), nil)
	active := evidenceMap(t, ev)["active"].([]any)[0].(map[string]any)
	want := map[string]any{
		"effect": "deny", "name": "weekend", "cron": "0 17 * * 5", "timezone": "Europe/London",
		"start": "2026-11-27T17:00:00Z", "end": "2026-11-30T08:00:00Z",
	}
	for k, v := range want {
		if active[k] != v {
			t.Errorf("active %s = %v, want %v", k, active[k], v)
		}
	}
}

func TestScheduleGateFactoryRejectsBadWindows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		gate config.Gate
	}{
		{name: "no windows", gate: scheduleGate("UTC", false)},
		{name: "bad cron", gate: scheduleGate("UTC", false, config.Window{Effect: config.WindowDeny, Cron: "@daily", Duration: config.Duration(time.Hour)})},
		{name: "bad zone", gate: scheduleGate("UTC", false, config.Window{Effect: config.WindowDeny, Cron: "0 9 * * *", Duration: config.Duration(time.Hour), Timezone: "Nowhere"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gates.NewRegistry()
			if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
				t.Fatalf("RegisterBuiltins: %v", err)
			}
			app := webApp()
			app.Gates = config.Gates{Before: []config.Gate{tt.gate}}
			if _, err := r.Plan(app); err == nil {
				t.Error("Plan accepted an invalid schedule")
			}
		})
	}
}

func TestScheduleGatePassIsSticky(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.clock.now = time.Date(2026, 11, 27, 16, 59, 0, 0, time.UTC)
	g := builtinGate(t, gates.Builtins{
		Tracked: f.tracked, Freezes: f.freezes, Applications: f.apps, Now: f.clock.Now,
	}, webApp(), scheduleGate("UTC", false, weekendFreeze))
	binding := gates.Binding{Name: "schedule", Type: config.GateSchedule, Kind: metrics.GateKindSchedule, Phase: core.GatePhaseBefore, Gate: g}
	h := newHarness(t, gates.Plan{Before: []gates.Binding{binding}}, func(o *gates.Options) { o.Now = f.clock.Now })
	d := deployment(core.KindDeploy)

	if out := mustEvaluate(t, h, d, core.GatePhaseBefore); out.Status != core.GatePassed {
		t.Fatalf("before the freeze = %s, want passed", out.Status)
	}
	f.clock.Advance(2 * time.Minute)
	if out := mustEvaluate(t, h, d, core.GatePhaseBefore); out.Status != core.GatePassed {
		t.Errorf("after the freeze started = %s, want the pass to stick", out.Status)
	}
	out, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseBefore)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if out.Status != core.GatePending {
		t.Errorf("Reevaluate during the freeze = %s, want pending", out.Status)
	}
}
