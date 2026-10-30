package vlapi

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/errreport"
)

// ErrorReportsState is the user's answer to sending error reports.
type ErrorReportsState struct {
	// Enabled is null until the user has answered, which is when the app asks.
	Enabled *bool `json:"enabled"`
	// Available is false in a build that cannot send reports at all, such as
	// a development build.
	Available bool `json:"available"`
}

// BugReportRequest is a bug report the user sends from the app or `vod`.
type BugReportRequest struct {
	Message     string `json:"message"`
	IncludeLogs bool   `json:"include_logs"`
	// SentFrom is "app", "web" or "cli".
	SentFrom string `json:"sent_from"`
	// AppVersion is the desktop app's version, when sent from it.
	AppVersion string `json:"app_version,omitempty"`
}

// BugReportResult identifies a sent bug report.
type BugReportResult struct {
	ID string `json:"id"`
}

// reportController is implemented by the daemon. Every reports.* subject is
// local-only: none is bridged to the dashboard.
type reportController interface {
	ErrorReports() ErrorReportsState
	SetErrorReports(enabled bool) error
	SendBugReport(BugReportRequest) (string, error)
}

// SetReportController installs the daemon's error and bug report handling.
// Called once during daemon startup.
func (s *Service) SetReportController(c reportController) {
	s.reportCtl = c
}

func (s *Service) handleGetErrorReports(req micro.Request) {
	if s.reportCtl == nil {
		respond(req, fail(errInternal, "report controller unavailable"))
		return
	}
	respond(req, OK(s.reportCtl.ErrorReports()))
}

func (s *Service) handleSetErrorReports(req micro.Request) {
	if s.reportCtl == nil {
		respond(req, fail(errInternal, "report controller unavailable"))
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(req.Data(), &body); err != nil || body.Enabled == nil {
		respond(req, fail(errInvalidArg, `expected {"enabled": true|false}`))
		return
	}
	if err := s.reportCtl.SetErrorReports(*body.Enabled); err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	respond(req, OK(s.reportCtl.ErrorReports()))
}

func (s *Service) handleSendBugReport(req micro.Request) {
	if s.reportCtl == nil {
		respond(req, fail(errInternal, "report controller unavailable"))
		return
	}
	var r BugReportRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	if strings.TrimSpace(r.Message) == "" {
		respond(req, fail(errInvalidArg, "describe the problem first"))
		return
	}
	id, err := s.reportCtl.SendBugReport(r)
	switch {
	case errors.Is(err, errreport.ErrUnavailable):
		respond(req, fail(errUnavailable, err.Error()))
		return
	case err != nil:
		respond(req, fail(errInternal, err.Error()))
		return
	}
	respond(req, OK(BugReportResult{ID: id}))
}
