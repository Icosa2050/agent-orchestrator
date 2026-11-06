package store_test

import (
	"context"
	"testing"
)

// Fork guard (Icosa2050, FORK.md): upstream #7440 added a daemon pass that
// permanently deletes archived sessions after session_retention_days. A fresh
// or migrated database must default to 0, which keeps every session, so no
// session is deleted unless the user chose a retention period.
func TestForkSessionRetentionDefaultsToKeep(t *testing.T) {
	s := newTestStore(t)
	settings, err := s.GetAppSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.SessionRetentionDays != 0 {
		t.Fatalf("SessionRetentionDays = %d on a fresh database; want 0 (keep archived sessions)", settings.SessionRetentionDays)
	}
}
