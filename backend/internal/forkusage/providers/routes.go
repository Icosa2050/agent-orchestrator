package providers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

// RoutePath is under the price book's prefix, so the LAN listener's
// device-credential gate for /api/v1/usage covers it.
const RoutePath = pricebook.RoutePrefix + "/providers"

// SettingsPath is the steering settings write route.
const SettingsPath = RoutePath + "/settings"

// Routes mounts:
//
//	GET  /api/v1/usage/fork/providers[?refresh=0]  provider rows (refreshes stale readings of enabled providers)
//	POST /api/v1/usage/fork/providers              {provider, enabled}; local only
//	POST /api/v1/usage/fork/providers/settings     {runwayLowDays?, balanceLow?, keyRemainingLowPct?, modelsListing?}; local only
func Routes(s *Service) func(chi.Router) {
	return func(r chi.Router) {
		r.Get(RoutePath, func(w http.ResponseWriter, req *http.Request) {
			refresh := req.URL.Query().Get("refresh") != "0"
			envelope.WriteJSON(w, http.StatusOK, s.Status(req.Context(), refresh))
		})
		r.Post(RoutePath, func(w http.ResponseWriter, req *http.Request) {
			// Enabling makes this daemon read the owner's key and send it to
			// the provider: a local decision only, never a remote one.
			if requestscope.IsLAN(req.Context()) {
				envelope.WriteAPIError(w, req, http.StatusForbidden, "forbidden", "PROVIDERS_LOCAL_ONLY",
					"provider polling can only be switched on this machine", nil)
				return
			}
			var body struct {
				Provider string `json:"provider"`
				Enabled  *bool  `json:"enabled"`
			}
			dec := json.NewDecoder(io.LimitReader(req.Body, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil || body.Enabled == nil {
				envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_BODY", "want {\"provider\": \"deepseek|openrouter\", \"enabled\": true|false}", nil)
				return
			}
			err := s.SetEnabled(body.Provider, *body.Enabled)
			switch {
			case errors.Is(err, errUnknownProvider):
				envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "UNKNOWN_PROVIDER", "provider must be deepseek or openrouter", nil)
				return
			case err != nil:
				envelope.WriteAPIError(w, req, http.StatusInternalServerError, "internal", "PROVIDERS_WRITE_FAILED", "could not save the setting", nil)
				return
			}
			envelope.WriteJSON(w, http.StatusOK, s.Status(req.Context(), *body.Enabled))
		})
		r.Post(SettingsPath, func(w http.ResponseWriter, req *http.Request) {
			if requestscope.IsLAN(req.Context()) {
				envelope.WriteAPIError(w, req, http.StatusForbidden, "forbidden", "PROVIDERS_LOCAL_ONLY",
					"steering settings can only be changed on this machine", nil)
				return
			}
			var body Settings
			dec := json.NewDecoder(io.LimitReader(req.Body, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil {
				envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_BODY",
					"want {\"runwayLowDays\"?, \"balanceLow\"?, \"keyRemainingLowPct\"?, \"modelsListing\"?}", nil)
				return
			}
			err := s.SetSettings(body)
			switch {
			case errors.Is(err, errBadSetting):
				envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_SETTING", err.Error(), nil)
				return
			case err != nil:
				envelope.WriteAPIError(w, req, http.StatusInternalServerError, "internal", "PROVIDERS_WRITE_FAILED", "could not save the setting", nil)
				return
			}
			envelope.WriteJSON(w, http.StatusOK, s.Status(req.Context(), false))
		})
	}
}
