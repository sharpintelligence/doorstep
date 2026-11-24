package windows

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	MaxDuration      = 366 * 24 * time.Hour
	maxChainedFiring = 1000
)

var (
	ErrInvalidCron     = errors.New("invalid cron expression")
	ErrInvalidDuration = errors.New("invalid window duration")
	ErrInvalidLocation = errors.New("invalid time zone")

	parser        = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	neverFiresRef = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

type Spec struct {
	Cron     string
	Duration time.Duration
	Location *time.Location
}

type Window struct {
	schedule cron.Schedule
	duration time.Duration
	location *time.Location
}

func LoadLocation(name string) (*time.Location, error) {
	switch name {
	case "":
		return time.UTC, nil
	case "Local":
		return nil, fmt.Errorf("%w: Local depends on the host; name an IANA zone such as Europe/London", ErrInvalidLocation)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not an IANA time zone name", ErrInvalidLocation, name)
	}
	return loc, nil
}

func Parse(s Spec) (Window, error) {
	if err := ValidateDuration(s.Duration); err != nil {
		return Window{}, err
	}
	if s.Location == nil {
		return Window{}, fmt.Errorf("%w: no location", ErrInvalidLocation)
	}
	schedule, err := parseCron(s.Cron)
	if err != nil {
		return Window{}, err
	}
	return Window{schedule: schedule, duration: s.Duration, location: s.Location}, nil
}

func ValidateCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

func ValidateDuration(d time.Duration) error {
	switch {
	case d <= 0:
		return fmt.Errorf("%w: %s must be positive", ErrInvalidDuration, d)
	case d > MaxDuration:
		return fmt.Errorf("%w: %s is longer than 366 days", ErrInvalidDuration, d)
	}
	return nil
}

func parseCron(raw string) (cron.Schedule, error) {
	expr := strings.TrimSpace(raw)
	switch {
	case expr == "":
		return nil, fmt.Errorf("%w: is empty", ErrInvalidCron)
	case strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ="):
		return nil, fmt.Errorf("%w: %q must not set a time zone; use the timezone field", ErrInvalidCron, raw)
	}
	schedule, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w (want five fields: minute hour day-of-month month day-of-week)", ErrInvalidCron, raw, err)
	}
	if schedule.Next(neverFiresRef).IsZero() {
		return nil, fmt.Errorf("%w: %q never fires", ErrInvalidCron, raw)
	}
	return schedule, nil
}

func (w Window) Duration() time.Duration {
	return w.duration
}

func (w Window) Active(now time.Time) (start, end time.Time, ok bool) {
	if w.schedule == nil {
		return time.Time{}, time.Time{}, false
	}
	first := w.next(now.Add(-w.duration))
	if first.IsZero() || first.After(now) {
		return time.Time{}, time.Time{}, false
	}
	end = first.Add(w.duration)
	next := first
	for range maxChainedFiring {
		next = w.next(next)
		if next.IsZero() || next.After(end) {
			break
		}
		end = next.Add(w.duration)
	}
	return first, end, true
}

func (w Window) Next(now time.Time) time.Time {
	if w.schedule == nil {
		return time.Time{}
	}
	return w.next(now)
}

func (w Window) next(t time.Time) time.Time {
	next := w.schedule.Next(t.In(w.location))
	if next.IsZero() {
		return next
	}
	return next.UTC()
}
