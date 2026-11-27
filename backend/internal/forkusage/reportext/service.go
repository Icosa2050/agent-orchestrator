package reportext

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Route is the report-ext path. It sits under /api/v1/usage like the price
// book routes, so the LAN listener's device-credential gate covers it.
const Route = pricebook.RoutePrefix + "/report-ext"

// Store is what the report reads: the fork DeepSeek query and the upstream
// report events (for every session's models).
type Store interface {
	ListForkDeepSeekUsageEvents(ctx context.Context, since, until time.Time, routes []string) ([]store.ForkDeepSeekUsageEvent, error)
	ListUsageReportEvents(ctx context.Context, since, until time.Time) ([]domain.UsageReportEvent, error)
}

// Service answers report-ext requests.
type Service struct {
	Store  Store
	Prices *pricebook.Service
}

// windowProbe is a report store that records the window the upstream report
// reader computes and returns no events, so report-ext uses upstream's exact
// window, periods and validation.
type windowProbe struct{ since, until time.Time }

func (p *windowProbe) ListUsageReportEvents(_ context.Context, since, until time.Time) ([]domain.UsageReportEvent, error) {
	p.since, p.until = since, until
	return nil, nil
}

// ReportWindow computes the upstream report window for the parameters.
func ReportWindow(ctx context.Context, days, offset int, timeZone string) (Window, error) {
	probe := &windowProbe{}
	rep, err := usagesvc.NewReportReader(probe, nil).Report(ctx, days, offset, timeZone, "")
	if err != nil {
		return Window{}, err
	}
	return Window{Days: rep.Days, TimeZone: rep.TimeZone, Resolution: rep.Resolution, Since: rep.Since, Until: rep.Until, Periods: rep.Periods}, nil
}

// Report builds the extension for one window.
func (s *Service) Report(ctx context.Context, days, offset int, timeZone string, projectID domain.ProjectID) (Report, error) {
	w, err := ReportWindow(ctx, days, offset, timeZone)
	if err != nil {
		return Report{}, err
	}
	book := s.Prices.Book()
	deepseek, err := s.Store.ListForkDeepSeekUsageEvents(ctx, w.Since, w.Until, book.Routes())
	if err != nil {
		return Report{}, err
	}
	all, err := s.Store.ListUsageReportEvents(ctx, w.Since, w.Until)
	if err != nil {
		return Report{}, err
	}
	rep, err := Build(Input{Window: w, Book: book, DeepSeek: deepseek, All: all, ProjectID: projectID})
	if err != nil {
		return Report{}, err
	}
	rep.PriceBook = bookRef(s.Prices.Status(), rep.Warnings)
	return rep, nil
}

// bookRef summarizes the price book for a report. A priced event in a year
// without a transcribed holiday calendar makes the report's prices stale,
// whatever the current year's state.
func bookRef(st pricebook.Status, reportWarnings []string) *BookRef {
	ref := &BookRef{Version: st.Version, Source: st.Source, Label: st.Label, EffectiveFrom: st.EffectiveFrom,
		LastCheckAt: st.LastCheckAt, LastCheckStatus: st.LastCheckStatus, Stale: st.Stale, Warnings: st.Warnings}
	for _, w := range reportWarnings {
		if w == pricebook.AssumptionYearMissed {
			ref.Stale = true
		}
	}
	return ref
}

// Routes mounts GET /api/v1/usage/fork/report-ext?days&offset&tz&projectId,
// with the upstream report's parameters and defaults (days 7).
func Routes(s *Service) func(chi.Router) {
	return func(r chi.Router) {
		r.Get(Route, func(w http.ResponseWriter, req *http.Request) {
			query := req.URL.Query()
			days, offset := 7, 0
			for _, p := range []struct {
				name   string
				target *int
			}{{"days", &days}, {"offset", &offset}} {
				if raw := query.Get(p.name); raw != "" {
					parsed, err := strconv.Atoi(raw)
					if err != nil {
						envelope.WriteError(w, req, apierr.Invalid("INVALID_USAGE_WINDOW", p.name+" must be a whole number", nil))
						return
					}
					*p.target = parsed
				}
			}
			rep, err := s.Report(req.Context(), days, offset, query.Get("tz"), domain.ProjectID(query.Get("projectId")))
			if err != nil {
				envelope.WriteError(w, req, err)
				return
			}
			envelope.WriteJSON(w, http.StatusOK, rep)
		})
	}
}
