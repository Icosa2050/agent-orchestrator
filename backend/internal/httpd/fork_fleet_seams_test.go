package httpd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

// Fork: fleet seam tests (FORK.md; design PR-0). They pin the seams in
// upstream-owned files: APIDeps.Extensions, BridgeService.OnDevicePaired and
// the handler that NewMobileLAN wraps inside its auth chain.

func routeTable(t *testing.T, r chi.Router) []string {
	t.Helper()
	var routes []string
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(routes)
	return routes
}

// An empty or nil Extensions list mounts nothing: the route table is identical.
func TestForkFleetExtensionsEmptyMountsNothing(t *testing.T) {
	build := func(ext []func(chi.Router)) []string {
		return routeTable(t, NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Extensions: ext}, ControlDeps{}))
	}
	want := build(nil)
	if len(want) == 0 {
		t.Fatal("empty route table")
	}
	for name, ext := range map[string][]func(chi.Router){"empty": {}, "nil entry": {nil}} {
		if got := build(ext); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s Extensions changed the route table", name)
		}
	}
	for _, route := range want {
		if strings.Contains(route, "/fleet/") {
			t.Fatalf("fleet route without an extension: %s", route)
		}
	}
}

// An extension is called once, after the API surface is registered, on the
// root router, so its routes get the full middleware stack.
func TestForkFleetExtensionsMountAfterAPIWithMiddleware(t *testing.T) {
	calls := 0
	var sawAPI bool
	ext := func(r chi.Router) {
		calls++
		sawAPI = r.Match(chi.NewRouteContext(), http.MethodGet, "/api/v1/sessions")
		r.Get("/api/v1/fleet/v1/capabilities", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Fleet-Request-ID", req.Header.Get("X-Request-Id"))
			w.WriteHeader(http.StatusOK)
		})
	}
	before := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})
	rec := httptest.NewRecorder()
	before.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet/v1/capabilities", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("fleet route without an extension: %d", rec.Code)
	}

	r := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Extensions: []func(chi.Router){ext}}, ControlDeps{})
	if calls != 1 || !sawAPI {
		t.Fatalf("extension calls=%d sawAPI=%v", calls, sawAPI)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/v1/capabilities", nil)
	req.Header.Set("X-Request-Id", "rid-1")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("extension route: %d", rec.Code)
	}
	// RequestID middleware ran before the extension handler.
	if rec.Header().Get("X-Fleet-Request-ID") != "rid-1" {
		t.Fatalf("extension route skipped the middleware stack: %v", rec.Header())
	}
	// The API surface is unchanged next to the extension.
	got := routeTable(t, r)
	want := append(routeTable(t, before), "GET /api/v1/fleet/v1/capabilities")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extension changed more than its own route")
	}
}

type pairedCall struct {
	code      string
	device    mobilebridge.AccessDevice
	persisted bool
}

type pairedHook struct {
	mu    sync.Mutex
	calls []pairedCall
	err   error
	// during runs inside the hook, after the device is stored.
	during func()
}

func (p *pairedHook) install(b *controllers.BridgeService) {
	b.OnDevicePaired = func(code string, device mobilebridge.AccessDevice) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		persisted := false
		for _, d := range b.AccessDevices() {
			if d.ID == device.ID {
				persisted = true
			}
		}
		p.calls = append(p.calls, pairedCall{code: code, device: device, persisted: persisted})
		if p.during != nil {
			p.during()
		}
		return p.err
	}
}

// A nil hook keeps pairing exactly as before; a hook sees the code (empty for
// the password) and the stored device, once, before the token is returned.
func TestForkOnDevicePairedHookArgumentsAndTiming(t *testing.T) {
	h := newPairingHost(t)
	if h.bridge.OnDevicePaired != nil {
		t.Fatal("default hook must be nil")
	}
	_, plainToken := h.pair(h.code(), "plain")
	if h.sessions(plainToken) != http.StatusOK {
		t.Fatal("nil hook: token does not work")
	}

	hook := &pairedHook{}
	hook.install(h.bridge)
	code := h.code()
	id, token := h.pair(code, "Phone")
	if len(hook.calls) != 1 {
		t.Fatalf("hook calls = %d", len(hook.calls))
	}
	call := hook.calls[0]
	if call.code != code || call.device.ID != id || call.device.Name != "Phone" || call.device.Platform != "ios" || !call.persisted {
		t.Fatalf("hook call = %#v, want code %q device %q persisted", call, code, id)
	}
	if call.device.TokenHash != mobilebridge.HashPassword(token) {
		t.Fatal("hook device does not match the returned token")
	}
	if h.sessions(token) != http.StatusOK {
		t.Fatal("token from a successful hook does not work")
	}

	h.pair("secret12", "Laptop")
	if len(hook.calls) != 2 || hook.calls[1].code != "" {
		t.Fatalf("password pairing hook call = %#v", hook.calls)
	}
	// Refused pairings never reach the hook.
	if status, _ := h.remote(http.MethodPost, devicePairPath, code, nil); status != http.StatusUnauthorized {
		t.Fatalf("reused code: %d", status)
	}
	if status, _ := h.remote(http.MethodPost, devicePairPath, token, nil); status != http.StatusForbidden {
		t.Fatalf("device token pairing: %d", status)
	}
	if len(hook.calls) != 2 {
		t.Fatalf("refused pairing reached the hook: %d calls", len(hook.calls))
	}
}

// U-G7 basis: a hook error revokes the device and fails the pairing, so no
// token is returned, and the spent code cannot be retried.
func TestForkOnDevicePairedFailureRevokesAndReturnsNoToken(t *testing.T) {
	h := newPairingHost(t)
	keptID, _ := h.pair(h.code(), "Kept")
	hook := &pairedHook{err: errors.New("grant store down")}
	hook.install(h.bridge)
	before := len(h.bridge.AccessDevices())
	code := h.code()
	status, body := h.remote(http.MethodPost, devicePairPath, code, map[string]string{"name": "Phone", "platform": "ios"})
	if status != http.StatusInternalServerError || body["code"] != "DEVICE_PAIRING" {
		t.Fatalf("hook failure: status=%d body=%v", status, body)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("token returned despite the hook failure: %v", body)
	}
	if strings.Contains(body["message"].(string), "grant store down") {
		t.Fatalf("internal hook error leaked to the device: %v", body)
	}
	if len(hook.calls) != 1 || !hook.calls[0].persisted {
		t.Fatalf("hook calls = %#v", hook.calls)
	}
	if got := len(h.bridge.AccessDevices()); got != before {
		t.Fatalf("device kept after hook failure: %d devices, want %d", got, before)
	}
	reloaded, err := mobilebridge.LoadAccessDevices(mobilebridge.AccessDevicesPath(filepath.Dir(filepath.Dir(h.bridge.ConfigPath))))
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	for _, d := range reloaded.List() {
		onDisk[d.ID] = true
	}
	if !onDisk[keptID] || onDisk[hook.calls[0].device.ID] {
		t.Fatalf("registry on disk = %v, want %s kept and %s revoked", onDisk, keptID, hook.calls[0].device.ID)
	}
	if status, _ := h.remote(http.MethodPost, devicePairPath, code, nil); status != http.StatusUnauthorized {
		t.Fatalf("spent code after a hook failure: %d", status)
	}
	// The same failure through the password path.
	if status, body := h.remote(http.MethodPost, devicePairPath, "secret12", nil); status != http.StatusInternalServerError || body["token"] != nil {
		t.Fatalf("password pairing with a failing hook: %d %v", status, body)
	}
}

// Even when the revoke after a hook failure fails too, the token never leaves
// PairDevice, and both errors are reported to the caller.
func TestForkOnDevicePairedFailureWithFailedRevokeStillWithholdsToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := mobilebridge.AccessDevicesPath(dir)
	devices, err := mobilebridge.LoadAccessDevices(path)
	if err != nil {
		t.Fatal(err)
	}
	codes := mobilebridge.NewPairingCodes(nil)
	bridge := &controllers.BridgeService{ConfigPath: mobilebridge.Path(dir), Devices: devices, Codes: codes, HostID: "h_test"}
	hookErr := errors.New("grant store down")
	bridge.OnDevicePaired = func(string, mobilebridge.AccessDevice) error {
		// Make the registry unwritable so the revoke cannot persist.
		if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
			t.Fatal(err)
		}
		return hookErr
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	code, _, err := codes.Issue()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := bridge.PairDevice(code, "Phone", "ios")
	if !errors.Is(err, hookErr) {
		t.Fatalf("err = %v, want the hook error", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the revoke failure joined", err)
	}
	if resp.Token != "" || resp.DeviceID != "" {
		t.Fatalf("response carries a credential: %#v", resp)
	}
}

// Issuance-path invariant (test-plan S30, design §4.3): every route that mints
// a device token goes through BridgeService.PairDevice and so through
// OnDevicePaired. Statically, the only call of the 3-argument
// AccessDevices.Issue in non-test backend code is inside
// BridgeService.PairDevice, only MobileController.PairDevice calls PairDevice,
// and only the router mounts that handler. A new minting path fails here.
func TestForkDeviceTokenIssuancePathInvariant(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..")) // the backend module
	if err != nil {
		t.Fatal(err)
	}
	type site struct{ file, fn string }
	var issue, issueValue, pair, pairHandler []site
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		// Only these files can name an *AccessDevices; elsewhere Issue is
		// usually a struct field (tracker issues).
		devicesInScope := file.Name.Name == "mobilebridge"
		for _, imp := range file.Imports {
			devicesInScope = devicesInScope || strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/mobilebridge")
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			calls := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						calls[sel] = true
						switch {
						case sel.Sel.Name == "Issue" && len(call.Args) == 3:
							issue = append(issue, site{rel, name})
						case sel.Sel.Name == "PairDevice":
							pair = append(pair, site{rel, name})
						}
					}
				}
				return true
			})
			// Method values (handler registrations, or an Issue taken as a
			// value to call later), not calls.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && !calls[sel] {
					switch sel.Sel.Name {
					case "PairDevice":
						pairHandler = append(pairHandler, site{rel, name})
					case "Issue":
						if !devicesInScope {
							return true
						}
						issueValue = append(issueValue, site{rel, name})
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := func(label string, got []site, allowed ...site) {
		t.Helper()
		if !reflect.DeepEqual(got, allowed) {
			t.Fatalf("%s sites = %v, want exactly %v (a new device-token path must go through BridgeService.PairDevice and OnDevicePaired)", label, got, allowed)
		}
	}
	in := func(parts ...string) string { return filepath.Join(append([]string{"internal", "httpd"}, parts...)...) }
	want("AccessDevices.Issue", issue, site{in("controllers", "mobile.go"), "BridgeService.PairDevice"})
	want("Issue method value", issueValue)
	want("PairDevice call", pair, site{in("controllers", "mobile.go"), "MobileController.PairDevice"})
	want("PairDevice handler", pairHandler, site{in("router.go"), "mountMobile"})
}

// The same invariant at runtime through the real router and LAN listener: the
// one device-token route reaches the hook, and a hook failure there withholds
// the token.
func TestForkDeviceTokenRouteRunsOnDevicePaired(t *testing.T) {
	dir := t.TempDir()
	devices, err := mobilebridge.LoadAccessDevices(mobilebridge.AccessDevicesPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	codes := mobilebridge.NewPairingCodes(nil)
	bridge := &controllers.BridgeService{ConfigPath: mobilebridge.Path(dir), HostID: "h_test", Devices: devices, Codes: codes}
	if err := mobilebridge.Save(bridge.ConfigPath, mobilebridge.State{Enabled: true, Password: "secret12", LoopbackOnly: true}); err != nil {
		t.Fatal(err)
	}
	hook := &pairedHook{}
	hook.install(bridge)
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Mobile: &controllers.MobileController{Bridge: bridge}, HostID: "h_test"}, ControlDeps{})
	lan := NewMobileLAN(router, "h_test", 0, nil, nil)
	lan.UseDeviceCredentials(devices, codes)
	bridge.LAN = lan
	lan.SetPasswordHash(mobilebridge.HashPassword("secret12"))
	port, err := lan.StartLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lan.Stop(context.Background()) })
	h := &pairingHost{t: t, bridge: bridge, lan: lan, router: router, port: port}
	_, token := h.pair(h.code(), "Phone")
	if len(hook.calls) != 1 || hook.calls[0].device.TokenHash != mobilebridge.HashPassword(token) {
		t.Fatalf("hook calls = %#v", hook.calls)
	}
	hook.err = errors.New("bind failed")
	if status, body := h.remote(http.MethodPost, devicePairPath, h.code(), nil); status != http.StatusInternalServerError || body["token"] != nil {
		t.Fatalf("failing hook through the real router: %d %v", status, body)
	}
	if got := len(bridge.AccessDevices()); got != 1 {
		t.Fatalf("devices = %d, want only the first", got)
	}
}

type gateCall struct {
	path       string
	credential requestscope.Credential
	lan        bool
}

// newGatedPairingHost is newPairingHost with wrap applied where the daemon
// applies its LAN gate hook: to the handler NewMobileLAN puts inside auth.
func newGatedPairingHost(t *testing.T, wrap func(http.Handler) http.Handler) *pairingHost {
	t.Helper()
	dir := t.TempDir()
	devices, err := mobilebridge.LoadAccessDevices(mobilebridge.AccessDevicesPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	codes := mobilebridge.NewPairingCodes(nil)
	bridge := &controllers.BridgeService{ConfigPath: mobilebridge.Path(dir), HostID: "h_test", Devices: devices, Codes: codes}
	if err := mobilebridge.Save(bridge.ConfigPath, mobilebridge.State{Enabled: true, Password: "secret12", LoopbackOnly: true}); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	mountMobile(router, &controllers.MobileController{Bridge: bridge})
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	for _, path := range []string{"/api/v1/sessions", "/api/v1/usage", "/api/v1/agents/accounts", "/api/v1/fleet/v1/capabilities", identityProbePath} {
		router.HandleFunc(path, ok)
	}
	lan := NewMobileLAN(wrap(router), bridge.HostID, 0, nil, nil)
	lan.UseDeviceCredentials(devices, codes)
	bridge.LAN = lan
	lan.SetPasswordHash(mobilebridge.HashPassword("secret12"))
	port, err := lan.StartLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lan.Stop(context.Background()) })
	return &pairingHost{t: t, bridge: bridge, lan: lan, router: router, port: port}
}

// LAN gate hook ordering: the wrapped handler runs inside the listener's
// control block and authMiddleware. It sees only authenticated requests (and
// the unauthenticated identity probe), with the accepted credential and device
// id in context. Refusals by the existing chain (no credential, the PR #24
// device-credential rule, the shared-password switch, loopback-only control
// routes) happen before it, so a gate can only narrow access, never widen it.
func TestForkFleetLANGateRunsAfterTheAuthChain(t *testing.T) {
	var mu sync.Mutex
	var calls []gateCall
	deny := false
	gate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, gateCall{path: r.URL.Path, credential: requestscope.CredentialFrom(r.Context()), lan: requestscope.IsLAN(r.Context())})
			refuse := deny && r.URL.Path == "/api/v1/fleet/v1/capabilities"
			mu.Unlock()
			if refuse {
				http.Error(w, "denied by gate", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := newGatedPairingHost(t, gate)
	reset := func() []gateCall {
		mu.Lock()
		defer mu.Unlock()
		out := calls
		calls = nil
		return out
	}
	expectNoGate := func(label string, status, want int) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: status %d, want %d", label, status, want)
		}
		if got := reset(); len(got) != 0 {
			t.Fatalf("%s reached the gate: %#v", label, got)
		}
	}

	id, token := h.pair(h.code(), "Phone")
	reset()

	// Refused before the gate.
	status, _ := h.remote(http.MethodGet, "/api/v1/sessions", "", nil)
	expectNoGate("no credential", status, http.StatusUnauthorized)
	status, _ = h.remote(http.MethodGet, "/api/v1/sessions", "wrong-password", nil)
	expectNoGate("wrong credential", status, http.StatusUnauthorized)
	for _, path := range []string{"/api/v1/usage", "/api/v1/agents/accounts"} {
		status, body := h.remote(http.MethodGet, path, "secret12", nil)
		if body["code"] != "DEVICE_CREDENTIAL_REQUIRED" {
			t.Fatalf("%s with the password: %v", path, body)
		}
		expectNoGate("shared password on "+path, status, http.StatusUnauthorized)
	}
	status, _ = h.remote(http.MethodPost, "/api/v1/mobile/pairing-code", token, nil)
	expectNoGate("loopback-only control route", status, http.StatusNotFound)

	// Reached with the accepted credential.
	if status, _ := h.remote(http.MethodGet, "/api/v1/sessions", token, nil); status != http.StatusOK {
		t.Fatalf("device token: %d", status)
	}
	got := reset()
	if len(got) != 1 || got[0].credential != (requestscope.Credential{Kind: requestscope.CredentialDevice, DeviceID: id}) || !got[0].lan {
		t.Fatalf("device request at the gate = %#v", got)
	}
	if status, _ := h.remote(http.MethodGet, "/api/v1/sessions", "secret12", nil); status != http.StatusOK {
		t.Fatalf("password on an ordinary route: %d", status)
	}
	if got := reset(); len(got) != 1 || got[0].credential.Kind != requestscope.CredentialPassword || got[0].credential.DeviceID != "" {
		t.Fatalf("password request at the gate = %#v", got)
	}
	// The identity probe skips auth by design; it is the only request the
	// gate sees without a credential.
	if status, _ := h.remote(http.MethodGet, identityProbePath, "", nil); status != http.StatusOK {
		t.Fatalf("identity probe: %d", status)
	}
	if got := reset(); len(got) != 1 || got[0].path != identityProbePath || got[0].credential != (requestscope.Credential{}) {
		t.Fatalf("identity probe at the gate = %#v", got)
	}

	// A gate can narrow: it refuses a route the chain allowed.
	mu.Lock()
	deny = true
	mu.Unlock()
	if status, _ := h.remote(http.MethodGet, "/api/v1/fleet/v1/capabilities", token, nil); status != http.StatusForbidden {
		t.Fatalf("gate refusal: %d", status)
	}
	reset()

	// The shared-password switch still refuses the password before the gate.
	if _, err := h.bridge.SetRequireDeviceCredentials(true); err != nil {
		t.Fatal(err)
	}
	status, body := h.remote(http.MethodGet, "/api/v1/sessions", "secret12", nil)
	if body["code"] != "DEVICE_CREDENTIAL_REQUIRED" {
		t.Fatalf("password with the switch on: %v", body)
	}
	expectNoGate("password with device credentials required", status, http.StatusUnauthorized)
}

// The identity wrap (the daemon's default) keeps the LAN surface unchanged:
// the same requests get the same answers with and without it.
func TestForkFleetLANGateIdentityIsNeutral(t *testing.T) {
	plain := newGatedPairingHost(t, func(h http.Handler) http.Handler { return h })
	_, plainToken := plain.pair(plain.code(), "Phone")
	type probe struct{ method, path, bearer string }
	probes := func(token string) []probe {
		return []probe{
			{http.MethodGet, "/api/v1/sessions", ""},
			{http.MethodGet, "/api/v1/sessions", "secret12"},
			{http.MethodGet, "/api/v1/sessions", token},
			{http.MethodGet, "/api/v1/usage", "secret12"},
			{http.MethodGet, "/api/v1/usage", token},
			{http.MethodGet, "/api/v1/fleet/v1/capabilities", token},
			{http.MethodPost, "/api/v1/mobile/pairing-code", token},
			{http.MethodGet, identityProbePath, ""},
		}
	}
	var want []int
	for _, p := range probes(plainToken) {
		status, _ := plain.remote(p.method, p.path, p.bearer, nil)
		want = append(want, status)
	}
	wrapped := 0
	other := newGatedPairingHost(t, func(h http.Handler) http.Handler {
		wrapped++
		return h
	})
	_, token := other.pair(other.code(), "Phone")
	var got []int
	for _, p := range probes(token) {
		status, _ := other.remote(p.method, p.path, p.bearer, nil)
		got = append(got, status)
	}
	if wrapped != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("identity wrap changed the LAN surface: wrapped=%d got %v want %v", wrapped, got, want)
	}
}
