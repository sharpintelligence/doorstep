package config

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/windows"
)

const (
	MaxScheduleWindows   = 100
	MaxWindowNameLength  = 100
	DefaultScheduleZone  = "UTC"
	windowEffectsChoices = "allow or deny"
)

type ScheduledWindow struct {
	Effect   WindowEffect
	Name     string
	Cron     string
	Timezone string
	Window   windows.Window
}

func (g Gate) ScheduleWindows() ([]ScheduledWindow, error) {
	out := make([]ScheduledWindow, 0, len(g.Windows))
	var errs []error
	for i, w := range g.Windows {
		parsed, err := w.parse(g.Timezone)
		if err != nil {
			errs = append(errs, fmt.Errorf("windows[%d]: %w", i, err))
			continue
		}
		out = append(out, parsed)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func (w Window) zone(gateZone string) string {
	return orDefault(orDefault(w.Timezone, gateZone), DefaultScheduleZone)
}

func (w Window) parse(gateZone string) (ScheduledWindow, error) {
	if err := validateWindowEffect(w.Effect); err != nil {
		return ScheduledWindow{}, err
	}
	zone := w.zone(gateZone)
	loc, err := windows.LoadLocation(zone)
	if err != nil {
		return ScheduledWindow{}, err
	}
	parsed, err := windows.Parse(windows.Spec{Cron: w.Cron, Duration: w.Duration.Std(), Location: loc})
	if err != nil {
		return ScheduledWindow{}, err
	}
	return ScheduledWindow{Effect: w.Effect, Name: w.Name, Cron: strings.TrimSpace(w.Cron), Timezone: zone, Window: parsed}, nil
}

func validateWindowEffect(e WindowEffect) error {
	switch e {
	case WindowAllow, WindowDeny:
		return nil
	case "":
		return fmt.Errorf("is required (want %s)", windowEffectsChoices)
	default:
		return fmt.Errorf("unknown effect %q (want %s)", e, windowEffectsChoices)
	}
}

func validateScheduleGate(r reporter, field string, g Gate) {
	if len(g.Windows) == 0 {
		r.addf(field+".windows", "at least one window is required for type %s", g.Type)
		return
	}
	if len(g.Windows) > MaxScheduleWindows {
		r.addf(field+".windows", "has %d windows, more than the limit of %d", len(g.Windows), MaxScheduleWindows)
	}
	if g.Timezone != "" {
		if _, err := windows.LoadLocation(g.Timezone); err != nil {
			r.check(field+".timezone", err)
		}
	}
	for i, w := range g.Windows {
		prefix := fmt.Sprintf("%s.windows[%d]", field, i)
		r.check(prefix+".effect", validateWindowEffect(w.Effect))
		r.check(prefix+".cron", windows.ValidateCron(w.Cron))
		r.check(prefix+".duration", windows.ValidateDuration(w.Duration.Std()))
		if w.Timezone != "" {
			if _, err := windows.LoadLocation(w.Timezone); err != nil {
				r.check(prefix+".timezone", err)
			}
		}
		r.check(prefix+".name", validateWindowName(w.Name))
	}
}

func validateWindowName(name string) error {
	switch {
	case !utf8.ValidString(name):
		return errors.New("must be valid UTF-8")
	case utf8.RuneCountInString(name) > MaxWindowNameLength:
		return fmt.Errorf("must be at most %d characters", MaxWindowNameLength)
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("must not contain control characters")
	}
	return nil
}

func forbidScheduleFields(r reporter, field string, g Gate) {
	if g.Windows != nil {
		r.addf(field+".windows", "not allowed for type %s", g.Type)
	}
	if g.Timezone != "" {
		r.addf(field+".timezone", "not allowed for type %s", g.Type)
	}
	if g.ApplyToRollbacks {
		r.addf(field+".apply_to_rollbacks", "not allowed for type %s", g.Type)
	}
}
