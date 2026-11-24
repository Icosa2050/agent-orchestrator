package pricebook

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"time"
	_ "time/tzdata" // Europe/Berlin display in packaged builds.

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// RoutePrefix sits under /api/v1/usage so the LAN listener's device-credential
// gate (httpd requiresDeviceCredential) covers these routes like upstream usage.
const RoutePrefix = "/api/v1/usage/fork"

// Routes mounts the price-book API on the daemon router:
//
//	GET  /api/v1/usage/fork/prices          price book status, versions, rates
//	POST /api/v1/usage/fork/prices/check    check the page now
//	POST /api/v1/usage/fork/prices/accept   record a held suspicious change {pageSha256}
//	POST /api/v1/usage/fork/prices/auto     switch the daily check {enabled}
//	GET  /api/v1/usage/fork/window?at=      peak/off-peak window at an instant (default now)
func Routes(s *Service) func(chi.Router) {
	return func(r chi.Router) {
		r.Get(RoutePrefix+"/prices", func(w http.ResponseWriter, _ *http.Request) {
			envelope.WriteJSON(w, http.StatusOK, s.Status())
		})
		r.Post(RoutePrefix+"/prices/check", func(w http.ResponseWriter, req *http.Request) {
			res, err := s.Check(req.Context())
			if err != nil {
				envelope.WriteAPIError(w, req, http.StatusInternalServerError, "internal", "PRICEBOOK_WRITE_FAILED", "could not save the price check result", nil)
				return
			}
			envelope.WriteJSON(w, http.StatusOK, res)
		})
		r.Post(RoutePrefix+"/prices/accept", func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				PageSHA256 string `json:"pageSha256"`
			}
			if !decodeBody(w, req, &body) {
				return
			}
			res, err := s.Accept(body.PageSHA256)
			switch {
			case errors.Is(err, errNoPending):
				envelope.WriteAPIError(w, req, http.StatusConflict, "conflict", "PRICEBOOK_NO_PENDING", err.Error(), nil)
			case err != nil:
				envelope.WriteAPIError(w, req, http.StatusInternalServerError, "internal", "PRICEBOOK_WRITE_FAILED", "could not record the price version", nil)
			default:
				envelope.WriteJSON(w, http.StatusOK, res)
			}
		})
		r.Post(RoutePrefix+"/prices/auto", func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Enabled *bool `json:"enabled"`
			}
			if !decodeBody(w, req, &body) {
				return
			}
			if body.Enabled == nil {
				envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_BODY", "enabled is required", nil)
				return
			}
			if err := s.SetAutoCheck(*body.Enabled); err != nil {
				envelope.WriteAPIError(w, req, http.StatusInternalServerError, "internal", "PRICEBOOK_WRITE_FAILED", "could not save the setting", nil)
				return
			}
			envelope.WriteJSON(w, http.StatusOK, s.Status())
		})
		r.Get(RoutePrefix+"/window", func(w http.ResponseWriter, req *http.Request) {
			at := s.cfg.Now()
			if raw := req.URL.Query().Get("at"); raw != "" && raw != "now" {
				parsed, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil {
					envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_TIME", "at must be an RFC 3339 time", nil)
					return
				}
				at = parsed
			}
			envelope.WriteJSON(w, http.StatusOK, s.WindowReport(at))
		})
	}
}

func decodeBody(w http.ResponseWriter, req *http.Request, out any) bool {
	dec := json.NewDecoder(io.LimitReader(req.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_BODY", "invalid JSON body", nil)
		return false
	}
	return true
}

// TimePair is one instant in UTC and Europe/Berlin.
type TimePair struct {
	UTC    time.Time `json:"utc"`
	Berlin string    `json:"berlin"`
}

// SpanPair is a window in UTC and Europe/Berlin.
type SpanPair struct {
	From  TimePair `json:"from"`
	Until TimePair `json:"until"`
}

// WindowModel is one model's price now and off-peak.
type WindowModel struct {
	Model         string `json:"model"`
	NowPerM       Rates  `json:"nowPerM"`
	PeakPerM      Rates  `json:"peakPerM"`
	OffPeakPerM   Rates  `json:"offPeakPerM"`
	RetiredOnPage bool   `json:"retiredOnPage,omitempty"`
}

// WindowReport is the "price now" answer for `ao usage window`.
type WindowReport struct {
	Provider        string        `json:"provider"`
	At              TimePair      `json:"at"`
	Window          string        `json:"window"`
	PriceFactor     string        `json:"priceFactor"`
	PeakVsOffPeak   string        `json:"peakVsOffPeak"`
	Reason          string        `json:"reason"`
	Holiday         string        `json:"holiday,omitempty"`
	Since           *TimePair     `json:"since,omitempty"`
	WindowEndsAt    *TimePair     `json:"windowEndsAt,omitempty"`
	NextOffPeak     *SpanPair     `json:"nextOffPeak,omitempty"`
	NextPeak        *SpanPair     `json:"nextPeak,omitempty"`
	WindowRule      string        `json:"windowRule"`
	HolidayCalendar string        `json:"holidayCalendar"`
	Assumptions     []string      `json:"assumptions,omitempty"`
	NoPriceEvidence bool          `json:"noPriceEvidence,omitempty"`
	Models          []WindowModel `json:"models"`
	PriceBook       PriceBookRef  `json:"priceBook"`
}

// PriceBookRef summarizes the price book behind an answer.
type PriceBookRef struct {
	Version         string     `json:"version"`
	Source          string     `json:"source"`
	Label           string     `json:"label,omitempty"`
	LastCheckAt     *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckStatus string     `json:"lastCheckStatus,omitempty"`
	Stale           bool       `json:"stale"`
	Warnings        []string   `json:"warnings"`
}

var berlin = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func pair(t time.Time) TimePair {
	return TimePair{UTC: t.UTC(), Berlin: t.In(berlin).Format(time.RFC3339)}
}

func spanPair(s *Span) *SpanPair {
	if s == nil {
		return nil
	}
	return &SpanPair{From: pair(s.From), Until: pair(s.Until)}
}

// WindowReport evaluates the window at t with the book in effect.
func (s *Service) WindowReport(t time.Time) WindowReport {
	book := s.Book()
	w := book.WindowAt(t)
	st := s.Status()
	rep := WindowReport{
		Provider: book.Provider, At: pair(w.At), Window: w.Window, PriceFactor: w.PriceFactor, Reason: w.Reason, Holiday: w.Holiday,
		NextOffPeak: spanPair(w.NextOffPeak), NextPeak: spanPair(w.NextPeak), WindowRule: WindowRule,
		HolidayCalendar: w.HolidayCalendar, Assumptions: w.Assumptions, NoPriceEvidence: w.NoPriceEvidence,
		Models: []WindowModel{},
		PriceBook: PriceBookRef{Version: w.Version, Source: st.Source, Label: st.Label, LastCheckAt: st.LastCheckAt,
			LastCheckStatus: st.LastCheckStatus, Stale: st.Stale, Warnings: st.Warnings},
	}
	if v, ok := book.versionAt(w.At); ok {
		rep.PriceBook.Source, rep.PriceBook.Label = v.Source, v.Label
		if v.multiple.Sign() > 0 {
			rep.PeakVsOffPeak = decimalString(new(big.Rat).Inv(v.multiple))
		}
		for _, r := range v.rates() {
			now := r.OffPeakPerM
			if w.Window == WindowPeak {
				now = r.PeakPerM
			}
			rep.Models = append(rep.Models, WindowModel{Model: r.Model, NowPerM: now, PeakPerM: r.PeakPerM, OffPeakPerM: r.OffPeakPerM, RetiredOnPage: r.RetiredOnPage})
		}
	}
	if w.Since != nil {
		p := pair(*w.Since)
		rep.Since = &p
	}
	if w.Until != nil {
		p := pair(*w.Until)
		rep.WindowEndsAt = &p
	}
	return rep
}
