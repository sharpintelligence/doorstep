package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validGateReport() GateReport {
	return GateReport{DeploymentID: 7, Gate: "e2e", Status: GatePassed, ReportedBy: "token:ci"}
}

func TestGateReportValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*GateReport)
		wantErr string
	}{
		{name: "minimal passed", mutate: func(*GateReport) {}},
		{name: "pending", mutate: func(r *GateReport) { r.Status = GatePending }},
		{name: "failed", mutate: func(r *GateReport) { r.Status = GateFailed }},
		{name: "https url", mutate: func(r *GateReport) { r.URL = "https://ci.example.com/runs/42" }},
		{name: "http url", mutate: func(r *GateReport) { r.URL = "http://ci.local:8080/runs/42?attempt=2" }},
		{name: "url at the limit", mutate: func(r *GateReport) {
			r.URL = "https://ci.example.com/" + strings.Repeat("a", MaxGateReportURLBytes-len("https://ci.example.com/"))
		}},
		{name: "details object", mutate: func(r *GateReport) { r.Details = json.RawMessage(`{"tests":12,"failed":0}`) }},
		{name: "empty details object", mutate: func(r *GateReport) { r.Details = json.RawMessage(`{}`) }},
		{name: "skipped", mutate: func(r *GateReport) { r.Status = GateSkipped }, wantErr: "status skipped cannot be reported"},
		{name: "unknown status", mutate: func(r *GateReport) { r.Status = "green" }, wantErr: `unknown status "green"`},
		{name: "empty status", mutate: func(r *GateReport) { r.Status = "" }, wantErr: `unknown status ""`},
		{name: "no deployment", mutate: func(r *GateReport) { r.DeploymentID = 0 }, wantErr: "invalid deployment id 0"},
		{name: "blank gate", mutate: func(r *GateReport) { r.Gate = "  " }, wantErr: "gate must not be empty"},
		{name: "blank reporter", mutate: func(r *GateReport) { r.ReportedBy = "" }, wantErr: "reported_by must not be empty"},
		{name: "javascript url", mutate: func(r *GateReport) { r.URL = "javascript:alert(1)" }, wantErr: "absolute http or https"},
		{name: "relative url", mutate: func(r *GateReport) { r.URL = "/runs/42" }, wantErr: "absolute http or https"},
		{name: "url without host", mutate: func(r *GateReport) { r.URL = "https:///runs" }, wantErr: "absolute http or https"},
		{name: "url with credentials", mutate: func(r *GateReport) { r.URL = "https://user:pw@ci.example.com/" }, wantErr: "without credentials"},
		{name: "unparsable url", mutate: func(r *GateReport) { r.URL = "https://ci.example.com/%zz" }, wantErr: "not a valid URL"},
		{name: "url over the limit", mutate: func(r *GateReport) {
			r.URL = "https://ci.example.com/" + strings.Repeat("a", MaxGateReportURLBytes)
		}, wantErr: "byte limit"},
		{name: "details array", mutate: func(r *GateReport) { r.Details = json.RawMessage(`[1,2]`) }, wantErr: "details must be a JSON object"},
		{name: "details string", mutate: func(r *GateReport) { r.Details = json.RawMessage(`"ok"`) }, wantErr: "details must be a JSON object"},
		{name: "details invalid json", mutate: func(r *GateReport) { r.Details = json.RawMessage(`{"a":`) }, wantErr: "details must be a JSON object"},
		{name: "details with NUL", mutate: func(r *GateReport) { r.Details = json.RawMessage(`{"a":"\u0000"}`) }, wantErr: "Postgres cannot store"},
		{name: "details over the limit", mutate: func(r *GateReport) {
			r.Details = json.RawMessage(`{"a":"` + strings.Repeat("x", MaxGateReportDetailsBytes) + `"}`)
		}, wantErr: "byte limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := validGateReport()
			tt.mutate(&r)
			err := r.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidGateReport) {
				t.Fatalf("Validate() = %v, want ErrInvalidGateReport", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
