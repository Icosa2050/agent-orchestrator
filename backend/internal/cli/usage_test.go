package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const usagePeaksJSON = `{"since":"2026-08-27T12:00:00Z","until":"2026-11-19T12:00:00Z","windows":[` +
	`{"provider":"claude","login":"me@example.com","email":"me@example.com","accountId":"device-claude","plan":"max","windowId":"seven_day","label":"Weekly",` +
	`"periods":[{"resetAt":"2026-11-12T08:00:00Z","peakUsedPct":91,"readings":900,"firstReadingAt":"2026-11-05T08:01:00Z","lastReadingAt":"2026-11-12T07:59:00Z","open":false},` +
	`{"resetAt":"2026-11-19T08:00:00Z","peakUsedPct":22,"readings":800,"firstReadingAt":"2026-11-12T08:01:00Z","lastReadingAt":"2026-11-19T07:58:00Z","open":false},` +
	`{"resetAt":"2026-11-26T08:00:00Z","peakUsedPct":5,"readings":200,"firstReadingAt":"2026-11-19T08:02:00Z","lastReadingAt":"2026-11-19T11:59:00Z","open":true}],` +
	`"days":[{"day":"2026-11-18","maxUsedPct":20,"readings":1400}],` +
	`"summary":{"periods":2,"avgPeak":56.5,"maxPeak":91,"periodsOver80":1,"periodsUnder30":1}}]}`

// usageReportJSON is what a current daemon answers: the period rows by default, the session rows for groupBy=session.
func usageReportJSON(groupBy, top string) string {
	topField := ""
	if top != "" {
		topField = `"top":` + top + `,`
	}
	if groupBy == "session" {
		return `{"days":7,"timeZone":"UTC","resolution":"day","since":"2026-11-18T00:00:00Z","until":"2026-11-25T00:00:00Z","periods":["2026-11-24"],"rows":[],"sessions":[],` + topField + `"groupBy":"session",` +
			`"sessionRows":[{"sessionId":"nebrivo-215","projectId":"nebrivo","harness":"codex","model":"gpt-6.1-sol","speed":"standard","events":9,"pricedEvents":9,"inputTokens":10,` +
			`"cacheReadTokens":90,"cacheWriteTokens":0,"outputTokens":5,"inputCostNanos":1,"cacheReadCostNanos":1,"cacheWriteCostNanos":0,"outputCostNanos":1,"costNanos":3,"premiumNanos":0,"cacheSavingsNanos":0}]}`
	}
	return `{"days":7,"timeZone":"UTC","resolution":"day","since":"2026-11-18T00:00:00Z","until":"2026-11-25T00:00:00Z","periods":["2026-11-24"],"rows":[],"sessions":[]` + strings.TrimSuffix(","+topField, ",") + `}`
}

func runUsageCLI(t *testing.T, args ...string) (string, []string, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/usage/fork/") { // Fork: usage seam (FORK.md): an old daemon without report-ext.
			http.NotFound(w, r)
			return
		}
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/usage/limit-peaks":
			_, _ = io.WriteString(w, usagePeaksJSON)
		case "/api/v1/usage/report":
			_, _ = io.WriteString(w, usageReportJSON(r.URL.Query().Get("groupBy"), r.URL.Query().Get("top")))
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
		{"usage", "limits", "--days", "0"},
		{"usage", "limits", "--days", "15"},
		{"usage", "limits", "--history", "--days", "7"},
	} {
		_, requests, err := runUsageCLI(t, args...)
		var usage usageError
		if !errors.As(err, &usage) || len(requests) != 0 {
			t.Fatalf("%v: err %v requests %v, want a usage error before any request", args, err, requests)
		}
	}
}

func TestUsageReportDefaultSendsNoGrouping(t *testing.T) {
	out, requests, err := runUsageCLI(t, "usage", "report", "--days", "3", "-p", "nebrivo")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0] != "/api/v1/usage/report?days=3&projectId=nebrivo" {
		t.Fatalf("requests = %v, want the old query with no groupBy or top", requests)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	_ = json.Unmarshal([]byte(usageReportJSON("", "")), &want)
	delete(got.(map[string]any), "ext") // Fork: usage seam (FORK.md): additive ext key.
	if gotJSON, _ := json.Marshal(got); string(gotJSON) != mustMarshalJSON(t, want) {
		t.Fatalf("default output changed:\n%s", out)
	}
	if strings.Contains(out, "sessionRows") {
		t.Fatalf("default output carries session rows:\n%s", out)
	}
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUsageReportBySessionTop(t *testing.T) {
	out, requests, err := runUsageCLI(t, "usage", "report", "--by", "session", "--top", "5", "--days", "7", "--offset", "1", "--tz", "Europe/Berlin", "--project", "nebrivo")
	if err != nil {
		t.Fatal(err)
	}
	want := "/api/v1/usage/report?days=7&groupBy=session&offset=1&projectId=nebrivo&top=5&tz=Europe%2FBerlin"
	if len(requests) != 1 || requests[0] != want {
		t.Fatalf("requests = %v, want %s", requests, want)
	}
	var got struct {
		GroupBy     string `json:"groupBy"`
		SessionRows []struct {
			SessionID string `json:"sessionId"`
			ProjectID string `json:"projectId"`
			Harness   string `json:"harness"`
			Model     string `json:"model"`
			CostNanos int64  `json:"costNanos"`
		} `json:"sessionRows"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.GroupBy != "session" || len(got.SessionRows) != 1 || got.SessionRows[0].SessionID != "nebrivo-215" ||
		got.SessionRows[0].ProjectID != "nebrivo" || got.SessionRows[0].Model != "gpt-6.1-sol" || got.SessionRows[0].CostNanos != 3 {
		t.Fatalf("output = %s", out)
	}
}

func TestUsageReportTopAloneAndExplicitPeriod(t *testing.T) {
	_, requests, err := runUsageCLI(t, "usage", "report", "--top", "3")
	if err != nil || requests[0] != "/api/v1/usage/report?top=3" {
		t.Fatalf("--top alone: %v %v", requests, err)
	}
	_, requests, err = runUsageCLI(t, "usage", "report", "--by", "period")
	if err != nil || requests[0] != "/api/v1/usage/report" {
		t.Fatalf("--by period: %v %v", requests, err)
	}
}

func TestUsageReportRejectsBadByAndTop(t *testing.T) {
	for _, args := range [][]string{
		{"usage", "report", "--by", "model"},
		{"usage", "report", "--top", "0"},
		{"usage", "report", "--top", "-2"},
		{"usage", "report", "--by", "session", "--top", "0"},
	} {
		_, requests, err := runUsageCLI(t, args...)
		var usage usageError
		if !errors.As(err, &usage) || len(requests) != 0 {
			t.Fatalf("%v: err %v requests %v, want a usage error before any request", args, err, requests)
		}
	}
}

func TestCheckReportAnswerCatchesAnOldDaemon(t *testing.T) {
	period, sessionTop := json.RawMessage(usageReportJSON("", "")), json.RawMessage(usageReportJSON("session", "5"))
	for _, tc := range []struct {
		name string
		data json.RawMessage
		by   string
		top  int
		want string
	}{
		{"default asks nothing", period, "", 0, ""},
		{"current daemon answers by session and top", sessionTop, "session", 5, ""},
		{"old daemon ignores groupBy", period, "session", 0, "--by session"},
		{"old daemon ignores top", period, "", 3, "--top"},
		{"top applied differently than asked", sessionTop, "session", 2, "--top"},
		{"not JSON", json.RawMessage(`not json`), "session", 0, "decode usage report"},
	} {
		err := checkReportAnswer(tc.data, tc.by, tc.top)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// TestCommandTreeHasNoDuplicateNames catches two merges each registering the
// same command: cobra accepts duplicate siblings and silently runs only one.
func TestCommandTreeHasNoDuplicateNames(t *testing.T) {
	root := NewRootCommand(Deps{})
	usage := 0
	for _, c := range root.Commands() {
		if c.Name() == "usage" {
			usage++
		}
	}
	if usage != 1 {
		t.Fatalf("root registers %d 'usage' commands, want exactly 1", usage)
	}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		seen := map[string]bool{}
		for _, c := range cmd.Commands() {
			if seen[c.Name()] {
				t.Errorf("%q registers %q more than once", cmd.CommandPath(), c.Name())
			}
			seen[c.Name()] = true
			walk(c)
		}
	}
	walk(root)
}
