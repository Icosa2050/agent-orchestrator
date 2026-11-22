package daemon

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

// forkFleet is the fork-only (Icosa2050, see FORK.md) wiring point for fleet
// messaging and control. It plugs into the fleet seams in upstream-owned
// files: APIDeps.Extensions (httpd), BridgeService.OnDevicePaired
// (controllers), the LAN handler wrap below, and CoordinatorDeps.Router
// (service/report).
//
// PR-0 only adds the seams: every method returns the inert default, so the
// daemon behaves exactly as it did before. The fleet package (grants, gate,
// ledger, routes, report hand-off) plugs in here from PR-A onwards.
type forkFleet struct{}

func newForkFleet() forkFleet { return forkFleet{} }

// routes returns the fork route mounts for APIDeps.Extensions. Nil mounts none.
func (forkFleet) routes() []func(chi.Router) { return nil }

// lanHandler wraps the shared router handler before the LAN listener puts it
// inside its control block and authMiddleware, so a wrapper sees the accepted
// credential (requestscope.CredentialFrom). The default returns h unchanged.
func (forkFleet) lanHandler(h http.Handler) http.Handler { return h }

// onDevicePaired is the BridgeService.OnDevicePaired hook. Nil does nothing.
func (forkFleet) onDevicePaired() func(code string, device mobilebridge.AccessDevice) error {
	return nil
}

// reportRouter is the report coordinator's Router. Nil keeps local delivery.
func (forkFleet) reportRouter() reportsvc.Router { return nil }
