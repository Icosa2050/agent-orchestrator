package telemetry

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func consentFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, OptInFile), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func newConsentedIdentity(t *testing.T, dir string) (*Identity, error) {
	consentFixture(t, dir)
	return NewIdentity(dir)
}
func newConsentedPostHogSink(t *testing.T, dir, key, host, version, agent string, client postHogClient, log *slog.Logger) (*PostHogSink, error) {
	consentFixture(t, dir)
	return NewPostHogSink(dir, key, host, version, agent, client, log)
}
