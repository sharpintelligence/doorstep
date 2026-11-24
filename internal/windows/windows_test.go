package windows_test

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/sharpintelligence/doorstep/internal/windows"
)

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := windows.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func mustParse(t *testing.T, expr string, d time.Duration, loc *time.Location) windows.Window {
	t.Helper()
	w, err := windows.Parse(windows.Spec{Cron: expr, Duration: d, Location: loc})
	if err != nil {
		t.Fatalf("Parse(%q, %s): %v", expr, d, err)
	}
	return w
}

func at(t *testing.T, loc *time.Location, value string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", value, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return ts
}

func TestLoadLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		zone    string
		want    string
		wantErr bool
	}{
		{name: "empty is UTC", zone: "", want: "UTC"},
		{name: "UTC", zone: "UTC", want: "UTC"},
		{name: "IANA zone", zone: "Europe/London", want: "Europe/London"},
		{name: "Local rejected", zone: "Local", wantErr: true},
		{name: "unknown zone", zone: "Mars/Olympus_Mons", wantErr: true},
		{name: "abbreviation", zone: "BST", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			loc, err := windows.LoadLocation(tt.zone)
			if tt.wantErr {
				if !errors.Is(err, windows.ErrInvalidLocation) {
					t.Fatalf("LoadLocation(%q) error = %v, want ErrInvalidLocation", tt.zone, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadLocation(%q): %v", tt.zone, err)
			}
			if loc.String() != tt.want {
				t.Errorf("LoadLocation(%q) = %s, want %s", tt.zone, loc, tt.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cron     string
		duration time.Duration
		location *time.Location
		want     error
	}{
		{name: "valid", cron: "0 17 * * 5", duration: time.Hour, location: time.UTC},
		{name: "ranges and steps", cron: "*/15 9-17 * 1-6 MON-FRI", duration: time.Minute, location: time.UTC},
		{name: "surrounding space", cron: "  0 9 * * *  ", duration: time.Hour, location: time.UTC},
		{name: "leap day", cron: "0 0 29 2 *", duration: time.Hour, location: time.UTC},
		{name: "maximum duration", cron: "0 0 1 1 *", duration: windows.MaxDuration, location: time.UTC},
		{name: "empty", cron: "", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "blank", cron: "   ", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "six fields", cron: "0 0 17 * * 5", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "four fields", cron: "0 17 * *", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "every descriptor", cron: "@every 1h", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "daily descriptor", cron: "@daily", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "TZ prefix", cron: "TZ=Europe/London 0 9 * * *", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "CRON_TZ prefix", cron: "CRON_TZ=UTC 0 9 * * *", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "out of range", cron: "60 9 * * *", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "never fires", cron: "0 0 30 2 *", duration: time.Hour, location: time.UTC, want: windows.ErrInvalidCron},
		{name: "zero duration", cron: "0 9 * * *", location: time.UTC, want: windows.ErrInvalidDuration},
		{name: "negative duration", cron: "0 9 * * *", duration: -time.Hour, location: time.UTC, want: windows.ErrInvalidDuration},
		{name: "duration too long", cron: "0 9 * * *", duration: windows.MaxDuration + time.Minute, location: time.UTC, want: windows.ErrInvalidDuration},
		{name: "no location", cron: "0 9 * * *", duration: time.Hour, want: windows.ErrInvalidLocation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, err := windows.Parse(windows.Spec{Cron: tt.cron, Duration: tt.duration, Location: tt.location})
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if w.Duration() != tt.duration {
					t.Errorf("Duration() = %s, want %s", w.Duration(), tt.duration)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Parse error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestActive(t *testing.T) {
	t.Parallel()

	london := mustLocation(t, "Europe/London")
	newYork := mustLocation(t, "America/New_York")
	tests := []struct {
		name      string
		cron      string
		duration  time.Duration
		location  *time.Location
		now       string
		wantOK    bool
		wantStart string
		wantEnd   string
	}{
		{name: "before start", cron: "0 9 * * *", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-23 08:59:59"},
		{name: "start is inclusive", cron: "0 9 * * *", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-23 09:00:00", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 17:00:00"},
		{name: "inside", cron: "0 9 * * *", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-23 12:30:00", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 17:00:00"},
		{name: "last instant inside", cron: "0 9 * * *", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-23 16:59:59", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 17:00:00"},
		{name: "end is exclusive", cron: "0 9 * * *", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-23 17:00:00"},
		{name: "weekday only skips weekend", cron: "0 9 * * 1-5", duration: 8 * time.Hour, location: time.UTC, now: "2026-11-22 12:00:00"},
		{name: "weekend freeze on Friday evening", cron: "0 17 * * 5", duration: 63 * time.Hour, location: time.UTC, now: "2026-11-27 18:00:00", wantOK: true, wantStart: "2026-11-27 17:00:00", wantEnd: "2026-11-30 08:00:00"},
		{name: "weekend freeze on Sunday", cron: "0 17 * * 5", duration: 63 * time.Hour, location: time.UTC, now: "2026-11-29 23:00:00", wantOK: true, wantStart: "2026-11-27 17:00:00", wantEnd: "2026-11-30 08:00:00"},
		{name: "weekend freeze over on Monday", cron: "0 17 * * 5", duration: 63 * time.Hour, location: time.UTC, now: "2026-11-30 08:00:00"},
		{name: "weekend freeze not yet on Friday", cron: "0 17 * * 5", duration: 63 * time.Hour, location: time.UTC, now: "2026-11-27 16:59:00"},
		{name: "overlapping firings extend end", cron: "0 9,10 * * *", duration: 90 * time.Minute, location: time.UTC, now: "2026-11-23 10:15:00", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 11:30:00"},
		{name: "contiguous firings chain", cron: "0 9,13 * * *", duration: 4 * time.Hour, location: time.UTC, now: "2026-11-23 10:00:00", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 17:00:00"},
		{name: "gap between firings does not chain", cron: "0 9,14 * * *", duration: 4 * time.Hour, location: time.UTC, now: "2026-11-23 10:00:00", wantOK: true, wantStart: "2026-11-23 09:00:00", wantEnd: "2026-11-23 13:00:00"},
		{name: "in the gap", cron: "0 9,14 * * *", duration: 4 * time.Hour, location: time.UTC, now: "2026-11-23 13:30:00"},
		{name: "local zone in winter", cron: "0 9 * * *", duration: time.Hour, location: newYork, now: "2026-01-15 14:30:00", wantOK: true, wantStart: "2026-01-15 14:00:00", wantEnd: "2026-01-15 15:00:00"},
		{name: "local zone in summer", cron: "0 9 * * *", duration: time.Hour, location: newYork, now: "2026-07-15 13:30:00", wantOK: true, wantStart: "2026-07-15 13:00:00", wantEnd: "2026-07-15 14:00:00"},
		{name: "local zone in summer outside", cron: "0 9 * * *", duration: time.Hour, location: newYork, now: "2026-07-15 14:30:00"},
		{name: "London spring forward day", cron: "0 9 * * *", duration: 8 * time.Hour, location: london, now: "2026-03-29 08:30:00", wantOK: true, wantStart: "2026-03-29 08:00:00", wantEnd: "2026-03-29 16:00:00"},
		{name: "London day before spring forward", cron: "0 9 * * *", duration: 8 * time.Hour, location: london, now: "2026-03-28 08:30:00"},
		{name: "London autumn back day", cron: "0 9 * * *", duration: 8 * time.Hour, location: london, now: "2026-10-25 16:30:00", wantOK: true, wantStart: "2026-10-25 09:00:00", wantEnd: "2026-10-25 17:00:00"},
		{name: "duration across the spring forward hour is elapsed time", cron: "0 0 * * *", duration: 3 * time.Hour, location: london, now: "2026-03-29 02:30:00", wantOK: true, wantStart: "2026-03-29 00:00:00", wantEnd: "2026-03-29 03:00:00"},
		{name: "after a duration across the spring forward hour", cron: "0 0 * * *", duration: 3 * time.Hour, location: london, now: "2026-03-29 03:00:00"},
		{name: "New York spring forward", cron: "0 9 * * *", duration: time.Hour, location: newYork, now: "2026-03-08 13:30:00", wantOK: true, wantStart: "2026-03-08 13:00:00", wantEnd: "2026-03-08 14:00:00"},
		{name: "New York fall back", cron: "0 9 * * *", duration: time.Hour, location: newYork, now: "2026-11-01 14:30:00", wantOK: true, wantStart: "2026-11-01 14:00:00", wantEnd: "2026-11-01 15:00:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := mustParse(t, tt.cron, tt.duration, tt.location)
			now := at(t, time.UTC, tt.now)
			start, end, ok := w.Active(now)
			if ok != tt.wantOK {
				t.Fatalf("Active(%s) ok = %v (start %s, end %s), want %v", now, ok, start, end, tt.wantOK)
			}
			if !ok {
				if !start.IsZero() || !end.IsZero() {
					t.Errorf("inactive window returned start %s end %s", start, end)
				}
				return
			}
			if want := at(t, time.UTC, tt.wantStart); !start.Equal(want) {
				t.Errorf("start = %s, want %s", start, want)
			}
			if want := at(t, time.UTC, tt.wantEnd); !end.Equal(want) {
				t.Errorf("end = %s, want %s", end, want)
			}
			if start.Location() != time.UTC || end.Location() != time.UTC {
				t.Errorf("start/end not in UTC: %s %s", start.Location(), end.Location())
			}
		})
	}
}

func TestActiveSubSecond(t *testing.T) {
	t.Parallel()

	w := mustParse(t, "0 9 * * *", time.Hour, time.UTC)
	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "just before start", now: time.Date(2026, 11, 23, 8, 59, 59, 999_999_999, time.UTC)},
		{name: "start", now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), want: true},
		{name: "just before end", now: time.Date(2026, 11, 23, 9, 59, 59, 999_999_999, time.UTC), want: true},
		{name: "end", now: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := w.Active(tt.now); ok != tt.want {
				t.Errorf("Active(%s) = %v, want %v", tt.now.Format(time.RFC3339Nano), ok, tt.want)
			}
		})
	}
}

func TestActiveChainIsCapped(t *testing.T) {
	t.Parallel()

	w := mustParse(t, "* * * * *", time.Minute, time.UTC)
	now := time.Date(2026, 11, 23, 12, 0, 30, 0, time.UTC)
	start, end, ok := w.Active(now)
	if !ok {
		t.Fatal("every-minute window is not active")
	}
	if want := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %s, want %s", start, want)
	}
	if want := start.Add(1001 * time.Minute); !end.Equal(want) {
		t.Errorf("end = %s, want %s", end, want)
	}
}

func TestNext(t *testing.T) {
	t.Parallel()

	london := mustLocation(t, "Europe/London")
	tests := []struct {
		name     string
		cron     string
		location *time.Location
		now      string
		want     string
	}{
		{name: "later today", cron: "0 9 * * *", location: time.UTC, now: "2026-11-23 08:00:00", want: "2026-11-23 09:00:00"},
		{name: "strictly after now", cron: "0 9 * * *", location: time.UTC, now: "2026-11-23 09:00:00", want: "2026-11-24 09:00:00"},
		{name: "next Monday", cron: "0 9 * * 1-5", location: time.UTC, now: "2026-11-28 10:00:00", want: "2026-11-30 09:00:00"},
		{name: "local zone", cron: "0 9 * * *", location: london, now: "2026-07-01 10:00:00", want: "2026-07-02 08:00:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := mustParse(t, tt.cron, time.Hour, tt.location)
			got := w.Next(at(t, time.UTC, tt.now))
			if want := at(t, time.UTC, tt.want); !got.Equal(want) {
				t.Errorf("Next = %s, want %s", got, want)
			}
		})
	}
}

func TestZeroWindow(t *testing.T) {
	t.Parallel()

	var w windows.Window
	if _, _, ok := w.Active(time.Now()); ok {
		t.Error("zero window is active")
	}
	if next := w.Next(time.Now()); !next.IsZero() {
		t.Errorf("zero window Next = %s, want zero", next)
	}
}
