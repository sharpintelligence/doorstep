package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	StatusContextPrefix = "doorstep/"

	MaxGateReportDetailsBytes = 32 << 10
	MaxGateReportURLBytes     = 2048
)

var (
	ErrGateDecided       = errors.New("gate already decided")
	ErrInvalidGateReport = errors.New("invalid gate report")
)

type GateReport struct {
	DeploymentID DeploymentID
	Gate         string
	Status       GateStatus
	Message      string
	URL          string
	Details      json.RawMessage
	ReportedBy   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func IsDoorstepStatusContext(context string) bool {
	return strings.HasPrefix(context, StatusContextPrefix)
}

func (r GateReport) Validate() error {
	var errs []error
	if r.DeploymentID <= 0 {
		errs = append(errs, fmt.Errorf("invalid deployment id %d", r.DeploymentID))
	}
	if strings.TrimSpace(r.Gate) == "" {
		errs = append(errs, errors.New("gate must not be empty"))
	}
	switch r.Status {
	case GatePending, GatePassed, GateFailed:
	case GateSkipped:
		errs = append(errs, errors.New("status skipped cannot be reported (want pending, passed or failed)"))
	default:
		errs = append(errs, fmt.Errorf("unknown status %q (want pending, passed or failed)", r.Status))
	}
	if err := validateReportURL(r.URL); err != nil {
		errs = append(errs, err)
	}
	if err := validateReportDetails(r.Details); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(r.ReportedBy) == "" {
		errs = append(errs, errors.New("reported_by must not be empty"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGateReport, err)
	}
	return nil
}

func validateReportURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MaxGateReportURLBytes {
		return fmt.Errorf("url is %d bytes, over the %d byte limit", len(raw), MaxGateReportURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("url must be an absolute http or https URL without credentials")
	}
	return nil
}

func validateReportDetails(details json.RawMessage) error {
	if len(details) == 0 {
		return nil
	}
	if len(details) > MaxGateReportDetailsBytes {
		return fmt.Errorf("details is %d bytes, over the %d byte limit", len(details), MaxGateReportDetailsBytes)
	}
	if !isJSONObject(details) {
		return errors.New("details must be a JSON object")
	}
	if err := ValidateGateEvidence(details); err != nil {
		return fmt.Errorf("details: %w", err)
	}
	return nil
}
