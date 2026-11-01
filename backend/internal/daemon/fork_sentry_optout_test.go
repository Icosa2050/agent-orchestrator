package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
)

// Fork guard (FORK.md): the telemetry_opt_out marker must stop daemon Sentry.
func TestOptOutMarkerStopsDaemonSentry(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	check := sentryPolicyHonoringOptOut(dir, func(context.Context) bool { calls++; return true })

	if !check(context.Background()) {
		t.Fatal("without the marker the policy decides; want capture allowed")
	}
	if err := os.WriteFile(filepath.Join(dir, telemetryadapter.OptOutFile), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if check(context.Background()) {
		t.Fatal("telemetry_opt_out present but daemon Sentry capture still allowed")
	}
	if calls != 0 {
		t.Fatalf("policy consulted %d times while opted out; the marker must win first", calls)
	}
	if err := os.Remove(filepath.Join(dir, telemetryadapter.OptOutFile)); err != nil {
		t.Fatal(err)
	}
	if !check(context.Background()) {
		t.Fatal("removing the marker must hand the decision back to the policy")
	}
}

func TestOptOutMarkerKeepsPolicyVeto(t *testing.T) {
	check := sentryPolicyHonoringOptOut(t.TempDir(), func(context.Context) bool { return false })
	if check(context.Background()) {
		t.Fatal("no marker and a policy veto must still keep capture off")
	}
}

func TestOptOutMarkerUncheckableFailsClosed(t *testing.T) {
	// A data dir path whose parent is a file: Stat fails with ENOTDIR, not ENOENT.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check := sentryPolicyHonoringOptOut(file, func(context.Context) bool { return true })
	if check(context.Background()) {
		t.Fatal("an opt-out marker that cannot be checked must keep capture off")
	}
}
