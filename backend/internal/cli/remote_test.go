package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeRemoteToken = "aod_remoteTokenValue1234567890"

type fakeSecrets struct {
	mu     sync.Mutex
	tokens map[string]string
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{tokens: map[string]string{}} }

func (f *fakeSecrets) Get(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, ok := f.tokens[name]
	if !ok {
		return "", errRemoteTokenMissing
	}
	return token, nil
}

func (f *fakeSecrets) Set(_ context.Context, name, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[name] = token
	return nil
}

func (f *fakeSecrets) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, name)
	return nil
}

func (f *fakeSecrets) Describe() string { return "the test store" }

// fakeRemoteHost mimics a host's authenticated LAN listener.
type fakeRemoteHost struct {
	srv      *httptest.Server
	hostID   string
	revoked  bool
	mu       sync.Mutex
	requests []string
	pairBody map[string]string
}

func (h *fakeRemoteHost) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	auth := "noauth"
	if r.Header.Get("Authorization") != "" {
		auth = r.Header.Get("Authorization")
	}
	h.requests = append(h.requests, r.Method+" "+r.URL.Path+" "+auth)
}

func (h *fakeRemoteHost) set(hostID string, revoked bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostID, h.revoked = hostID, revoked
}

func (h *fakeRemoteHost) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func newFakeRemoteHost(t *testing.T, hostID string) *fakeRemoteHost {
	t.Helper()
	h := &fakeRemoteHost{hostID: hostID}
	apiErr := func(w http.ResponseWriter, status int, code, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg, "requestId": "req-1"})
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.record(r)
		h.mu.Lock()
		hostID, revoked := h.hostID, h.revoked
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/identity" {
			_ = json.NewEncoder(w).Encode(map[string]any{"hostId": hostID, "apiVersion": 1})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/remote-host/devices" {
			if r.Header.Get("Authorization") != "Bearer CODE1234" {
				apiErr(w, http.StatusUnauthorized, "PAIRING_CODE_USED", "the pairing code was already used")
				return
			}
			body, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			_ = json.Unmarshal(body, &h.pairBody)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"hostId": hostID, "deviceId": "dev_cli", "token": fakeRemoteToken, "credential": "pairing-code"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeRemoteToken {
			apiErr(w, http.StatusUnauthorized, "BAD_PASSWORD", "missing or invalid connection password")
			return
		}
		if revoked {
			apiErr(w, http.StatusUnauthorized, "DEVICE_REVOKED", "this device was removed from the host; pair it again")
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]any{{"id": "remote-proj", "name": "Remote", "path": "/srv/remote"}}})
		case "GET /api/v1/sessions":
			sessions := []map[string]any{}
			if r.URL.Query().Get("active") == "true" {
				sessions = append(sessions, map[string]any{"id": "remote-1", "projectId": "remote-proj", "kind": "worker", "status": "working",
					"createdAt": "2026-11-19T08:00:00Z", "updatedAt": "2026-11-19T08:00:00Z"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
		case "GET /api/v1/usage/limit-windows":
			_ = json.NewEncoder(w).Encode(map[string]any{"since": "2026-11-12T00:00:00Z", "until": "2026-11-19T00:00:00Z", "logins": []any{}})
		case "GET /api/v1/usage/limit-peaks":
			_, _ = io.WriteString(w, usagePeaksJSON)
		case "GET /api/v1/agents/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{"id": "remote-acct", "provider": "codex", "email": hostID + "@example.com", "signedIn": true}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

type remoteTestEnv struct {
	dir     string
	secrets *fakeSecrets
	deps    Deps
}

func newRemoteTestEnv(t *testing.T) *remoteTestEnv {
	t.Helper()
	setConfigEnv(t) // no run file: the local daemon is stopped
	t.Setenv("AO_HOST", "")
	env := &remoteTestEnv{dir: t.TempDir(), secrets: newFakeSecrets()}
	env.deps = Deps{
		ProcessAlive:  func(int) bool { return true },
		RemotesDir:    env.dir,
		RemoteSecrets: env.secrets,
		Now:           func() time.Time { return time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC) },
	}
	return env
}

func (e *remoteTestEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, _, err := executeCLI(t, e.deps, args...)
	return out, err
}

func (e *remoteTestEnv) add(t *testing.T, name string, host *fakeRemoteHost) {
	t.Helper()
	if out, err := e.run(t, "remote", "add", name, host.srv.URL, "--code", "CODE1234"); err != nil {
		t.Fatalf("remote add: %v\n%s", err, out)
	}
}

func TestRemoteAddExchangesCodeForADeviceTokenKeptOutOfConfig(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	out, err := env.run(t, "remote", "add", "mini", host.srv.URL, "--code", "CODE1234")
	if err != nil {
		t.Fatalf("remote add: %v", err)
	}
	if !strings.Contains(out, `Paired "mini" with host host-r`) || !strings.Contains(out, "dev_cli") {
		t.Fatalf("output = %q", out)
	}
	if env.secrets.tokens["mini"] != fakeRemoteToken {
		t.Fatalf("token not stored in the secret store: %v", env.secrets.tokens)
	}
	hostname, _ := os.Hostname()
	if name, want := host.pairBody["name"], cliDeviceLabel(hostname); name != want {
		t.Fatalf("device name = %q, want %q", name, want)
	}
	if got := host.seen()[0]; got != "GET /api/v1/identity noauth" {
		t.Fatalf("first request = %q; the identity probe must carry no credential", got)
	}
	path := filepath.Join(env.dir, "cli-remotes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), fakeRemoteToken) || strings.Contains(string(data), "CODE1234") {
		t.Fatalf("cli-remotes.json holds a secret: %s", data)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); fileModesEnforced && perm != 0o600 {
		t.Fatalf("cli-remotes.json mode = %o, want 600", perm)
	}
	var file cliRemotesFile
	if err := json.Unmarshal(data, &file); err != nil || len(file.Remotes) != 1 || file.Remotes[0].HostID != "host-r" || file.Remotes[0].URL != host.srv.URL {
		t.Fatalf("metadata = %+v (%v)", file, err)
	}

	// A second add under the same name needs --force.
	if _, err := env.run(t, "remote", "add", "mini", host.srv.URL, "--code", "CODE1234"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("duplicate add err = %v", err)
	}
}

func TestRemoteAddRefusesAUsedCodeAndUntrustedNetworks(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	_, err := env.run(t, "remote", "add", "mini", host.srv.URL, "--code", "STALE")
	if err == nil || !strings.Contains(err.Error(), "ao remote-host pair") || !strings.Contains(err.Error(), "PAIRING_CODE_USED") {
		t.Fatalf("used code err = %v", err)
	}
	if len(env.secrets.tokens) != 0 {
		t.Fatal("a refused pairing stored a token")
	}

	_, err = env.run(t, "remote", "add", "lan", "http://192.0.2.10:3011", "--code", "CODE1234")
	if err == nil || !strings.Contains(err.Error(), "Tailscale") {
		t.Fatalf("untrusted network err = %v", err)
	}
	_, err = env.run(t, "remote", "add", "mini", "http://mini:3011/api", "--code", "CODE1234")
	if ExitCode(err) != 2 {
		t.Fatalf("URL with a path: exit %d, err %v", ExitCode(err), err)
	}
	_, err = env.run(t, "remote", "add", "mini", host.srv.URL)
	if ExitCode(err) != 2 {
		t.Fatalf("missing --code: exit %d, err %v", ExitCode(err), err)
	}
}

func TestNormalizeRemoteURLAndTrustedNetworks(t *testing.T) {
	u, err := normalizeRemoteURL("http://100.101.102.103")
	if err != nil || u.String() != "http://100.101.102.103:3011" {
		t.Fatalf("got %v, %v", u, err)
	}
	ctx := context.Background()
	for _, raw := range []string{"http://100.101.102.103:3011", "http://127.0.0.1:3011", "https://mini.tail1234.ts.net", "http://[fd7a:115c:a1e0::1]:3011", "http://localhost:3011"} {
		u, err := normalizeRemoteURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkRemoteNetwork(ctx, u, false); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://192.168.1.20:3011", "https://203.0.113.9"} {
		u, _ := normalizeRemoteURL(raw)
		if err := checkRemoteNetwork(ctx, u, false); err == nil {
			t.Fatalf("%s was trusted", raw)
		}
		if err := checkRemoteNetwork(ctx, u, true); err != nil {
			t.Fatalf("%s with explicit allow: %v", raw, err)
		}
	}
}

func TestHostFlagReadsWithTheDeviceToken(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)

	out, err := env.run(t, "--host", "mini", "project", "ls", "--json")
	if err != nil {
		t.Fatalf("project ls --host: %v", err)
	}
	if !strings.Contains(out, "remote-proj") {
		t.Fatalf("output = %q", out)
	}
	seen := host.seen()
	if last := seen[len(seen)-1]; last != "GET /api/v1/projects Bearer "+fakeRemoteToken {
		t.Fatalf("last request = %q", last)
	}

	t.Setenv("AO_HOST", "mini")
	out, err = env.run(t, "usage", "limits", "--days", "7")
	if err != nil {
		t.Fatalf("usage limits --days via AO_HOST: %v", err)
	}
	var limits map[string]any
	if err := json.Unmarshal([]byte(out), &limits); err != nil || limits["logins"] == nil {
		t.Fatalf("usage limits --days output = %q (%v)", out, err)
	}
	if last := host.seen()[len(host.seen())-1]; last != "GET /api/v1/usage/limit-windows Bearer "+fakeRemoteToken {
		t.Fatalf("last request = %q", last)
	}
	out, err = env.run(t, "usage", "limits")
	if err != nil || !strings.Contains(out, "codex host-r@example.com") {
		t.Fatalf("usage limits via AO_HOST = %q (%v)", out, err)
	}
	out, err = env.run(t, "usage", "limits", "--history")
	if err != nil || !strings.Contains(out, "claude me@example.com") || !strings.Contains(out, "5%*") {
		t.Fatalf("usage limits --history via AO_HOST = %q (%v)", out, err)
	}
}

func TestHostFlagIsReadOnly(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)
	before := len(host.seen())

	_, err := env.run(t, "--host", "mini", "session", "kill", "remote-1")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("kill --host: exit %d, err %v", ExitCode(err), err)
	}
	t.Setenv("AO_HOST", "mini")
	if _, err := env.run(t, "project", "rm", "remote-proj", "--yes"); ExitCode(err) != 2 {
		t.Fatalf("project rm with AO_HOST: exit %d, err %v", ExitCode(err), err)
	}
	if got := host.seen(); len(got) != before {
		t.Fatalf("a refused command reached the host: %v", got[before:])
	}
	// `ao remote` itself ignores AO_HOST.
	if _, err := env.run(t, "remote", "ls", "--json"); err != nil {
		t.Fatalf("remote ls with AO_HOST: %v", err)
	}

	// The transport refuses mutations even if a command opted in by mistake.
	target, err := (&commandContext{deps: env.deps.withDefaults()}).openRemote(context.Background(), "mini")
	if err != nil {
		t.Fatal(err)
	}
	c := &commandContext{deps: env.deps.withDefaults(), remote: target}
	if err := c.postJSON(context.Background(), "sessions/remote-1/kill", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("POST over remote err = %v", err)
	}
}

func TestRevokedRemoteTokenAsksToRePair(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)
	host.set("host-r", true)

	_, err := env.run(t, "--host", "mini", "project", "ls")
	var authErr remoteAuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "Re-pair") || !strings.Contains(err.Error(), "DEVICE_REVOKED") {
		t.Fatalf("err = %v", err)
	}
	out, err := env.run(t, "remote", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listing struct{ Remotes []remoteHealth }
	if err := json.Unmarshal([]byte(out), &listing); err != nil || len(listing.Remotes) != 1 || listing.Remotes[0].Status != remoteStatusNeedsRepair {
		t.Fatalf("remote ls = %s (%v)", out, err)
	}
}

func TestOfflineAndMismatchedRemotes(t *testing.T) {
	env := newRemoteTestEnv(t)
	offline := newFakeRemoteHost(t, "host-off")
	moved := newFakeRemoteHost(t, "host-moved")
	env.add(t, "off", offline)
	env.add(t, "moved", moved)
	offline.srv.Close()
	moved.set("host-other", false)
	before := len(moved.seen())

	_, err := env.run(t, "--host", "off", "project", "ls")
	if err == nil || !strings.Contains(err.Error(), "offline or unreachable") {
		t.Fatalf("offline err = %v", err)
	}
	_, err = env.run(t, "--host", "moved", "project", "ls")
	if err == nil || !strings.Contains(err.Error(), "token was not sent") {
		t.Fatalf("mismatch err = %v", err)
	}
	for _, req := range moved.seen()[before:] {
		if !strings.HasSuffix(req, "noauth") {
			t.Fatalf("token sent to a mismatched host: %q", req)
		}
	}
}

func TestStatusAndSessionsFleetJSON(t *testing.T) {
	env := newRemoteTestEnv(t)
	good := newFakeRemoteHost(t, "host-good")
	gone := newFakeRemoteHost(t, "host-gone")
	revoked := newFakeRemoteHost(t, "host-revoked")
	env.add(t, "good", good)
	env.add(t, "gone", gone)
	env.add(t, "revoked", revoked)
	gone.srv.Close()
	revoked.set("host-revoked", true)

	out, err := env.run(t, "status", "--fleet", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var status struct{ Hosts []remoteHealth }
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := map[string]string{}
	for _, h := range status.Hosts {
		got[h.Name] = h.Status
	}
	want := map[string]string{"local": remoteStatusOffline, "good": remoteStatusOK, "gone": remoteStatusOffline, "revoked": remoteStatusNeedsRepair}
	for name, st := range want {
		if got[name] != st {
			t.Fatalf("fleet status = %v, want %v", got, want)
		}
	}
	if !status.Hosts[0].Local || status.Hosts[0].Name != "local" {
		t.Fatalf("local host must come first: %+v", status.Hosts[0])
	}

	out, err = env.run(t, "session", "ls", "--fleet", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var sessions struct {
		Hosts []struct {
			Name   string
			Status string
			Data   []sessionListEntry
		}
	}
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	found := false
	for _, h := range sessions.Hosts {
		if h.Name == "good" {
			found = h.Status == remoteStatusOK && len(h.Data) == 1 && h.Data[0].ID == "remote-1"
		}
	}
	if !found {
		t.Fatalf("session ls --fleet = %s", out)
	}

	text, err := env.run(t, "status", "--fleet")
	if err != nil || !strings.Contains(text, "needs re-pair") || !strings.Contains(text, "(this machine)") {
		t.Fatalf("text fleet status = %q (%v)", text, err)
	}
	if _, err := env.run(t, "--host", "good", "status", "--fleet"); ExitCode(err) != 2 {
		t.Fatalf("--host with --fleet: exit %d, err %v", ExitCode(err), err)
	}
}

func TestRemoteRmDeletesTheLocalToken(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)
	out, err := env.run(t, "remote", "rm", "mini")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ao remote-host devices revoke dev_cli") {
		t.Fatalf("output = %q", out)
	}
	if len(env.secrets.tokens) != 0 {
		t.Fatalf("token kept: %v", env.secrets.tokens)
	}
	if _, err := env.run(t, "--host", "mini", "project", "ls"); ExitCode(err) != 2 {
		t.Fatalf("removed host still usable: %v", err)
	}
}

func TestFileSecretStoreIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ao", "cli-remote-tokens.json")
	store := fileSecretStore{path: path}
	ctx := context.Background()
	if _, err := store.Get(ctx, "mini"); !errors.Is(err, errRemoteTokenMissing) {
		t.Fatalf("empty store err = %v", err)
	}
	if err := store.Set(ctx, "mini", fakeRemoteToken); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || (fileModesEnforced && info.Mode().Perm() != 0o600) {
		t.Fatalf("token file mode = %v (%v)", info.Mode().Perm(), err)
	}
	if got, err := store.Get(ctx, "mini"); err != nil || got != fakeRemoteToken {
		t.Fatalf("get = %q, %v", got, err)
	}
	// Windows has no POSIX mode bits to loosen or check; the file relies on
	// the user-profile ACL there.
	if fileModesEnforced {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(ctx, "mini"); err == nil {
			t.Fatal("a world-readable token file was accepted")
		}
		_ = os.Chmod(path, 0o600)
	}
	if err := store.Delete(ctx, "mini"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "mini"); !errors.Is(err, errRemoteTokenMissing) {
		t.Fatalf("after delete err = %v", err)
	}
}

func TestKeychainStoreKeepsTheTokenOffArgvAndVerifiesTheWrite(t *testing.T) {
	saved := ""
	var calls [][]string
	store := newKeychainSecretStore("security", "/home/u/.ao/dev/data")
	account := "mini@" + store.scope
	store.run = func(_ context.Context, stdin string, args ...string) ([]byte, int, error) {
		calls = append(calls, args)
		switch args[0] {
		case "-i":
			if strings.Contains(stdin, `-s "icosa-orchestrator-cli" -a "`+account+`"`) {
				saved = fakeRemoteToken
			}
			return nil, 0, nil
		case "find-generic-password":
			if saved == "" {
				return nil, 44, nil
			}
			return []byte(saved + "\n"), 0, nil
		case "delete-generic-password":
			saved = ""
			return nil, 0, nil
		}
		return nil, 1, nil
	}
	ctx := context.Background()
	if err := store.Set(ctx, "mini", fakeRemoteToken); err != nil {
		t.Fatal(err)
	}
	for _, args := range calls {
		if strings.Contains(strings.Join(args, " "), fakeRemoteToken) {
			t.Fatalf("token on argv: %v", args)
		}
	}
	if got, err := store.Get(ctx, "mini"); err != nil || got != fakeRemoteToken {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := store.Delete(ctx, "mini"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "mini"); !errors.Is(err, errRemoteTokenMissing) {
		t.Fatalf("after delete err = %v", err)
	}
	// security -i exits 0 even when the write failed; the read-back catches it.
	failing := keychainSecretStore{security: "security", run: func(_ context.Context, _ string, args ...string) ([]byte, int, error) {
		if args[0] == "find-generic-password" {
			return nil, 44, nil
		}
		return nil, 0, nil
	}}
	if err := failing.Set(ctx, "mini", fakeRemoteToken); err == nil {
		t.Fatal("an unverified keychain write succeeded")
	}
}

func TestRemoteHTTPClientBypassesEnvironmentProxies(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	c := &commandContext{deps: DefaultDeps()}
	client := c.remoteHTTPClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("remote transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("remote client transport has a proxy func; tokens could travel through HTTP(S)_PROXY")
	}
	if c.deps.HTTPClient.Transport != nil {
		t.Fatal("the shared daemon client was modified")
	}
	if client.CheckRedirect == nil {
		t.Fatal("remote client follows redirects")
	}

	injected := &http.Transport{}
	c = &commandContext{deps: Deps{HTTPClient: &http.Client{Transport: injected}}}
	if got := c.remoteHTTPClient(time.Second).Transport; got != injected {
		t.Fatalf("injected transport replaced: %T", got)
	}
}

func TestInheritedAOHostIsIgnoredInsideAgentSessions(t *testing.T) {
	env := newRemoteTestEnv(t)
	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)
	before := len(host.seen())

	cfg := setConfigEnv(t)
	var mu sync.Mutex
	var local []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		local = append(local, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}})
		case "POST /api/v1/sessions/local-1/kill":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "local-1"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(daemon.Close)
	writeRunFileFor(t, cfg, daemon)
	t.Setenv("AO_HOST", "mini")
	t.Setenv("AO_SESSION_ID", "agent-orchestrator-1")

	if _, err := env.run(t, "session", "ls", "--json"); err != nil {
		t.Fatalf("session ls in an agent session: %v", err)
	}
	if _, err := env.run(t, "session", "kill", "local-1"); ExitCode(err) == 2 {
		t.Fatalf("kill refused in an agent session: %v", err)
	}
	mu.Lock()
	reached := strings.Join(local, ",")
	mu.Unlock()
	if !strings.Contains(reached, "GET /api/v1/sessions") || !strings.Contains(reached, "POST /api/v1/sessions/local-1/kill") {
		t.Fatalf("local daemon saw %q", reached)
	}
	if got := host.seen(); len(got) != before {
		t.Fatalf("an inherited AO_HOST reached the remote: %v", got[before:])
	}

	// An explicit --host still works inside a session.
	if out, err := env.run(t, "--host", "mini", "project", "ls", "--json"); err != nil || !strings.Contains(out, "remote-proj") {
		t.Fatalf("--host in an agent session: %q, %v", out, err)
	}
}

func TestRemotesLiveInTheDaemonDataDirSharedWithAgents(t *testing.T) {
	env := newRemoteTestEnv(t)
	env.deps.RemotesDir = ""
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("AO_DATA_DIR", "")   // a user terminal: no override
	profileDataDir := filepath.Join(home, ".ao", "dev", "data")

	host := newFakeRemoteHost(t, "host-r")
	env.add(t, "mini", host)
	if _, err := os.Stat(filepath.Join(profileDataDir, "cli-remotes.json")); err != nil {
		t.Fatalf("cli-remotes.json not in the profile data dir: %v", err)
	}
	if store, err := (&commandContext{deps: Deps{LookPath: func(string) (string, error) { return "", errors.New("none") }}}).remoteSecrets(); err != nil ||
		store.Describe() != (fileSecretStore{path: filepath.Join(profileDataDir, "cli-remote-tokens.json")}).Describe() {
		t.Fatalf("token file store = %v, %v", store, err)
	}

	names := func() []string {
		t.Helper()
		out, err := env.run(t, "remote", "ls", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var listing struct{ Remotes []remoteHealth }
		if err := json.Unmarshal([]byte(out), &listing); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range listing.Remotes {
			got = append(got, r.Name)
		}
		return got
	}
	// An agent of the same daemon gets AO_DATA_DIR set to that data dir.
	t.Setenv("AO_DATA_DIR", profileDataDir)
	if got := names(); len(got) != 1 || got[0] != "mini" {
		t.Fatalf("agent sees remotes %v, want [mini]", got)
	}
	// An isolated run sees none.
	t.Setenv("AO_DATA_DIR", t.TempDir())
	if got := names(); len(got) != 0 {
		t.Fatalf("isolated run sees remotes %v", got)
	}
}

func TestKeychainAccountsAreScopedToTheDataDir(t *testing.T) {
	accounts := map[string]string{}
	for _, dir := range []string{"/home/u/.ao/dev/data", "/tmp/scratch"} {
		store := newKeychainSecretStore("security", dir)
		var seen []string
		store.run = func(_ context.Context, stdin string, args ...string) ([]byte, int, error) {
			seen = append(seen, stdin+" "+strings.Join(args, " "))
			if args[0] == "find-generic-password" {
				return []byte(fakeRemoteToken), 0, nil
			}
			return nil, 0, nil
		}
		ctx := context.Background()
		if err := store.Set(ctx, "mini", fakeRemoteToken); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "mini"); err != nil {
			t.Fatal(err)
		}
		want := `-a "mini@` + store.scope + `"`
		for _, call := range seen {
			if !strings.Contains(call, want) && !strings.Contains(call, "-a mini@"+store.scope) {
				t.Fatalf("call %q does not use account mini@%s", call, store.scope)
			}
		}
		accounts[dir] = store.account("mini")
	}
	if accounts["/home/u/.ao/dev/data"] == accounts["/tmp/scratch"] {
		t.Fatalf("two data dirs share keychain account %q", accounts["/tmp/scratch"])
	}
	if got := newKeychainSecretStore("security", "/tmp/scratch/").account("mini"); got != accounts["/tmp/scratch"] {
		t.Fatalf("an equivalent path changed the account: %q vs %q", got, accounts["/tmp/scratch"])
	}
}

func TestUsageLimitsFleet(t *testing.T) {
	env := newRemoteTestEnv(t)
	good := newFakeRemoteHost(t, "host-good")
	gone := newFakeRemoteHost(t, "host-gone")
	env.add(t, "good", good)
	env.add(t, "gone", gone)
	gone.srv.Close()

	out, err := env.run(t, "usage", "limits", "--fleet", "--history", "--weeks", "4", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Hosts []struct {
			Name   string
			Status string
			Data   *usageLimitPeaksResponse
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := map[string]string{}
	for _, h := range res.Hosts {
		got[h.Name] = h.Status
		if h.Name == "good" && (h.Data == nil || len(h.Data.Windows) != 1) {
			t.Fatalf("good host history = %s", out)
		}
	}
	if got["local"] != remoteStatusOffline || got["good"] != remoteStatusOK || got["gone"] != remoteStatusOffline {
		t.Fatalf("fleet hosts = %v", got)
	}
	if last := good.seen()[len(good.seen())-1]; !strings.HasPrefix(last, "GET /api/v1/usage/limit-peaks ") {
		t.Fatalf("last request = %q", last)
	}

	text, err := env.run(t, "usage", "limits", "--fleet")
	if err != nil || !strings.Contains(text, "== good ") || !strings.Contains(text, "codex host-good@example.com") {
		t.Fatalf("usage limits --fleet = %q (%v)", text, err)
	}

	out, err = env.run(t, "usage", "report", "--fleet")
	if err != nil || !strings.Contains(out, `"hosts"`) {
		t.Fatalf("usage report --fleet = %q (%v)", out, err)
	}
	if _, err := env.run(t, "--host", "good", "usage", "limits", "--fleet"); ExitCode(err) != 2 {
		t.Fatalf("--host with --fleet: exit %d, err %v", ExitCode(err), err)
	}
}

func TestCLIDeviceLabelUsesAShortSanitizedHostname(t *testing.T) {
	long := strings.Repeat("a", 100)
	for _, tc := range []struct{ host, want string }{
		{"mini", "mini CLI"},
		{"Bernhards-MacBook.local", "Bernhards-MacBook CLI"},
		{"sjc22-bt145-aedfa60b-be16-4309-812f-7895c43e1bab-16C5EB835B88.local", "sjc22-bt145-aedfa60b-be16-4309-812f-7895c43e1bab-16C5EB835B8 CLI"},
		{"WIN RUNNER (1)", "WIN-RUNNER-1 CLI"},
		{long, long[:remoteHostLinkLabelMaxBytes-len(" CLI")] + " CLI"},
		{"  ", "Unknown host CLI"},
		{".local", "Unknown host CLI"},
	} {
		got := cliDeviceLabel(tc.host)
		if got != tc.want || len(got) > remoteHostLinkLabelMaxBytes {
			t.Errorf("cliDeviceLabel(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
