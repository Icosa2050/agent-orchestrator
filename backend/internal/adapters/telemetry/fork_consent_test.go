package telemetry

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestForkFreshInstallDoesNotExport(t *testing.T) {
	calls := 0
	sink, err := NewPostHogSink(t.TempDir(), "phc_test", "https://posthog.test", "1.0.0", "", roundTripClient(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	sink.Emit(context.Background(), ports.TelemetryEvent{Name: "ao.app.installed", Source: "daemon", OccurredAt: time.Now(), Level: ports.TelemetryLevelInfo})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("fresh install sent %d requests", calls)
	}
	if !sink.Identity().OptedOut() {
		t.Fatal("fresh install must be opted out")
	}
}

func TestForkAnalyticsConsentFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "symlink", "unsafe", "uncheckable", "optout"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, OptInFile)
			switch kind {
			case "malformed":
				if err := os.WriteFile(file, []byte("true"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(dir, "missing"), file); err != nil {
					t.Skip(err)
				}
			case "unsafe":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX permissions")
				}
				if err := os.WriteFile(file, []byte("1\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "uncheckable":
				if err := os.WriteFile(file, nil, 0600); err != nil {
					t.Fatal(err)
				}
				dir = file
			case "optout":
				if err := os.WriteFile(file, []byte("1\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, OptOutFile)); err != nil {
					t.Skip(err)
				}
			}
			if AnalyticsConsentGranted(dir) {
				t.Fatal("unsafe or absent consent allowed export")
			}
		})
	}
}
