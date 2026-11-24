package config

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestGateScheduleWindows(t *testing.T) {
	t.Parallel()

	friday := time.Date(2026, 11, 27, 17, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		gate       Gate
		wantErr    []string
		wantZones  []string
		wantActive []bool
	}{
		{
			name: "parses every window with its own zone",
			gate: Gate{Type: GateSchedule, Timezone: "Europe/London", Windows: []Window{
				{Effect: WindowDeny, Cron: " 0 17 * * 5 ", Duration: Duration(63 * time.Hour), Name: "weekend"},
				{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour), Timezone: "America/New_York"},
			}},
			wantZones:  []string{"Europe/London", "America/New_York"},
			wantActive: []bool{true, true},
		},
		{
			name: "no zones means UTC",
			gate: Gate{Type: GateSchedule, Windows: []Window{
				{Effect: WindowAllow, Cron: "0 18 * * *", Duration: Duration(time.Hour)},
			}},
			wantZones:  []string{"UTC"},
			wantActive: []bool{false},
		},
		{
			name: "errors name the window",
			gate: Gate{Type: GateSchedule, Windows: []Window{
				{Effect: WindowAllow, Cron: "0 9 * * *", Duration: Duration(time.Hour)},
				{Effect: "maybe", Cron: "0 9 * * *", Duration: Duration(time.Hour)},
				{Effect: WindowDeny, Cron: "0 9 * *", Duration: Duration(time.Hour)},
				{Effect: WindowDeny, Cron: "0 9 * * *", Duration: Duration(time.Hour), Timezone: "Local"},
				{Effect: WindowDeny, Cron: "0 9 * * *"},
			}},
			wantErr: []string{
				`windows[1]: unknown effect "maybe"`,
				"windows[2]: invalid cron expression",
				"windows[3]: invalid time zone",
				"windows[4]: invalid window duration",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.gate.ScheduleWindows()
			assertErrorContains(t, err, tt.wantErr)
			if err != nil {
				return
			}
			if len(got) != len(tt.gate.Windows) {
				t.Fatalf("ScheduleWindows() returned %d windows, want %d", len(got), len(tt.gate.Windows))
			}
			for i, w := range got {
				src := tt.gate.Windows[i]
				if w.Effect != src.Effect || w.Name != src.Name || w.Timezone != tt.wantZones[i] || w.Window.Duration() != src.Duration.Std() {
					t.Errorf("window %d = %+v, want from %+v in %s", i, w, src, tt.wantZones[i])
				}
				if _, _, active := w.Window.Active(friday); active != tt.wantActive[i] {
					t.Errorf("window %d active at %s = %v, want %v", i, friday, active, tt.wantActive[i])
				}
			}
		})
	}
}
