package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const usagePeaksJSON = `{"since":"2026-08-27T12:00:00Z","until":"2026-11-19T12:00:00Z","windows":[` +
	`{"provider":"claude","login":"me@example.com","email":"me@example.com","accountId":"device-claude","plan":"max","windowId":"seven_day","label":"Weekly",` +
	`"periods":[{"resetAt":"2026-11-12T08:00:00Z","peakUsedPct":91,"readings":900,"firstReadingAt":"2026-11-05T08:01:00Z","lastReadingAt":"2026-11-12T07:59:00Z","open":false},` +
	`{"resetAt":"2026-11-19T08:00:00Z","peakUsedPct":22,"readings":800,"firstReadingAt":"2026-11-12T08:01:00Z","lastReadingAt":"2026-11-19T07:58:00Z","open":false},` +
	`{"resetAt":"2026-11-26T08:00:00Z","peakUsedPct":5,"readings":200,"firstReadingAt":"2026-11-19T08:02:00Z","lastReadingAt":"2026-11-19T11:59:00Z","open":true}],` +
	`"days":[{"day":"2026-11-18","maxUsedPct":20,"readings":1400}],` +
	`"summary":{"periods":2,"avgPeak":56.5,"maxPeak":91,"periodsOver80":1,"periodsUnder30":1}}]}`

func runUsageCLI(t *testing.T, args ...string) (string, []string, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/usage/limit-peaks":
			_, _ = io.WriteString(w, usagePeaksJSON)
		case "/api/v1/agents/accounts":
			_, _ = io.WriteString(w, `{"accounts":[{"id":"device-claude","provider":"claude","email":"me@example.com","plan":"max","signedIn":true,`+
				`"limits":{"windows":[{"id":"seven_day","label":"Weekly","usedPercent":42,"resetsAt":"2026-11-26T08:00:00Z"}],"fetchedAt":"2026-11-19T11:59:00Z"}},`+
				`{"id":"device-codex","provider":"codex","signedIn":false}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	if err != nil {
		t.Logf("stderr: %s", errOut)
	}
	return out, requests, err
}

func TestUsageLimitsHistoryJSON(t *testing.T) {
	out, requests, err := runUsageCLI(t, "usage", "limits", "--history", "--weeks", "4", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0] != "/api/v1/usage/limit-peaks?weeks=4" {
		t.Fatalf("requests = %v", requests)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	_ = json.Unmarshal([]byte(usagePeaksJSON), &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON changed on the way through:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestUsageLimitsHistoryTable(t *testing.T) {
	out, requests, err := runUsageCLI(t, "usage", "limits", "--history")
	if err != nil {
		t.Fatal(err)
	}
	if requests[0] != "/api/v1/usage/limit-peaks?weeks=12" {
		t.Fatalf("requests = %v, want the default 12 weeks", requests)
	}
	for _, want := range []string{"claude me@example.com", "Weekly", "56%", "91%", "91%, ", "5%*"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUsageLimitsCurrent(t *testing.T) {
	out, requests, err := runUsageCLI(t, "usage", "limits")
	if err != nil {
		t.Fatal(err)
	}
	if requests[0] != "/api/v1/agents/accounts" {
		t.Fatalf("requests = %v", requests)
	}
	for _, want := range []string{"claude me@example.com", "Weekly", "42%", "codex device-codex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUsageLimitsRejectsBadWeeks(t *testing.T) {
	for _, args := range [][]string{
		{"usage", "limits", "--history", "--weeks", "0"},
		{"usage", "limits", "--history", "--weeks", "27"},
		{"usage", "limits", "--weeks", "4"},
	} {
		_, requests, err := runUsageCLI(t, args...)
		var usage usageError
		if !errors.As(err, &usage) || len(requests) != 0 {
			t.Fatalf("%v: err %v requests %v, want a usage error before any request", args, err, requests)
		}
	}
}
