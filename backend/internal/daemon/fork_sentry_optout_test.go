package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
	"github.com/aoagents/agent-orchestrator/backend/internal/telemetrymeta"
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

type optOutRecordingTransport struct{ events chan *sentry.Event }

func (*optOutRecordingTransport) Configure(sentry.ClientOptions)        {}
func (*optOutRecordingTransport) Flush(time.Duration) bool              { return true }
func (*optOutRecordingTransport) FlushWithContext(context.Context) bool { return true }
func (*optOutRecordingTransport) Close()                                {}
func (t *optOutRecordingTransport) SendEvent(event *sentry.Event)       { t.events <- event }

// Fork guard (FORK.md): every daemon Sentry entry point, including the
// background reporting upstream added in #7682, sends nothing while the
// telemetry_opt_out marker exists, even when the policy file allows capture.
func TestOptOutMarkerStopsEveryDaemonSentryPath(t *testing.T) {
	dir := t.TempDir()
	transport := &optOutRecordingTransport{events: make(chan *sentry.Event, 8)}
	if err := sentryobs.Init(sentryobs.Config{DSN: "https://public@example.com/1", InstallID: "ins_sampled_5", Release: "1.0.0", Environment: "stable", Transport: transport}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sentryobs.SetPolicyCheck(nil)
		_ = sentryobs.Init(sentryobs.Config{})
	})
	sentryobs.SetPolicyCheck(sentryPolicyHonoringOptOut(dir, func(context.Context) bool { return true }))
	marker := filepath.Join(dir, telemetryadapter.OptOutFile)
	if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	failure := errors.New("boom")
	sentryobs.CaptureBackground(ctx, failure, "reaper", "reaper_tick", nil)
	sentryobs.CaptureBackground(ctx, failure, "session_manager", "chat_spawn", map[string]string{"agent": "claude-code"})
	sentryobs.CaptureErrorAsync(ctx, failure, map[string]string{"operation": "http"}, "fork_optout_async", telemetrymeta.SafeRoute{}, telemetrymeta.FailureSite{})
	sentryobs.CaptureError(ctx, failure, map[string]string{"operation": "http"}, "fork_optout_sync", telemetrymeta.SafeRoute{}, telemetrymeta.FailureSite{})
	sentryobs.CapturePanic(ctx, map[string]string{"operation": "background_panic"}, "fork_optout_panic", telemetrymeta.SafeRoute{}, telemetrymeta.FailureSite{})
	if err := sentryobs.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-transport.events:
		t.Fatalf("telemetry_opt_out present but daemon Sentry sent %+v", event)
	case <-time.After(200 * time.Millisecond):
	}

	// Positive control: the same capture goes out once the marker is gone, so
	// the silence above comes from the marker and not from a broken harness.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	sentryobs.CaptureBackground(ctx, failure, "reaper", "reaper_tick", nil)
	select {
	case <-transport.events:
	case <-time.After(5 * time.Second):
		t.Fatal("without the marker the background capture never arrived; the opt-out test proves nothing")
	}
}
