package daemon

import (
	"context"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
)

// sentryPolicyHonoringOptOut is a fork-only guard (Icosa2050, see FORK.md): the
// telemetry_opt_out marker in the data dir stops daemon Sentry capture as well
// as PostHog export, whatever telemetry_policy.json says. The marker is read
// before every capture, so an opt-out applies without a restart. A marker that
// cannot be checked counts as present.
func sentryPolicyHonoringOptOut(dataDir string, policy func(context.Context) bool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		if telemetryadapter.OptOutMarkerPresent(dataDir) {
			return false
		}
		return policy(ctx)
	}
}
