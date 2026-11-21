package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const backgroundSessionJSON = `{"id":"demo-1","projectId":"demo","kind":"worker","activity":{"state":"active","lastActivityAt":"2026-06-02T12:00:00Z"},` +
	`"backgroundWork":{"pending":true,"items":[{"kind":"review","id":"run-1","type":"claude-code"},{"kind":"agent_task","id":"bash-1","type":"local_bash","description":"npm test"}]},` +
	`"isTerminated":false,"createdAt":"2026-06-02T11:00:00Z","updatedAt":"2026-06-02T12:00:00Z","status":"working","prs":[]}`

// A fleet watcher reads `ao session ls/get --json`; a worker waiting on its
// own background work must carry what it waits on, not read as idle.
func TestSessionJSONCarriesBackgroundWork(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions":
			if r.URL.Query().Get("active") == "false" {
				_, _ = io.WriteString(w, `{"sessions":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"sessions":[`+backgroundSessionJSON+`]}`)
		case "/api/v1/sessions/demo-1":
			_, _ = io.WriteString(w, `{"session":`+backgroundSessionJSON+`}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	deps := Deps{ProcessAlive: func(int) bool { return true }}

	out, errOut, err := executeCLI(t, deps, "session", "ls", "--project", "demo", "--json")
	if err != nil {
		t.Fatalf("session ls --json: %v\n%s", err, errOut)
	}
	var list struct {
		Data []struct {
			ID             string `json:"id"`
			Activity       string `json:"activity"`
			BackgroundWork *struct {
				Pending bool `json:"pending"`
				Items   []struct {
					Kind string `json:"kind"`
				} `json:"items"`
			} `json:"backgroundWork"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decode ls: %v\n%s", err, out)
	}
	if len(list.Data) != 1 || list.Data[0].Activity != "active" || list.Data[0].BackgroundWork == nil ||
		!list.Data[0].BackgroundWork.Pending || len(list.Data[0].BackgroundWork.Items) != 2 ||
		list.Data[0].BackgroundWork.Items[0].Kind != "review" {
		t.Fatalf("ls --json = %s", out)
	}

	out, errOut, err = executeCLI(t, deps, "session", "get", "demo-1", "--json")
	if err != nil {
		t.Fatalf("session get --json: %v\n%s", err, errOut)
	}
	var got sessionResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Session.BackgroundWork == nil || len(got.Session.BackgroundWork.Items) != 2 ||
		got.Session.BackgroundWork.Items[1].Description != "npm test" {
		t.Fatalf("get --json backgroundWork = %+v", got.Session.BackgroundWork)
	}

	out, _, err = executeCLI(t, deps, "session", "get", "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "waiting on: review claude-code, agent_task local_bash: npm test") {
		t.Fatalf("get output missing background work:\n%s", out)
	}
}
