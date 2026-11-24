package daemon

import (
	"net/http"
	"testing"
)

type markerHandler struct{}

func (*markerHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// PR-0: the fork fleet wiring is inert. Every seam gets its no-op default, so
// the daemon behaves exactly as before the seams existed.
func TestForkFleetWiringIsInert(t *testing.T) {
	fleet := newForkFleet()
	if routes := fleet.routes(); routes != nil {
		t.Fatalf("routes = %d mounts, want nil", len(routes))
	}
	if hook := fleet.onDevicePaired(); hook != nil {
		t.Fatal("onDevicePaired hook is set")
	}
	if router := fleet.reportRouter(); router != nil {
		t.Fatalf("report router = %#v, want a nil interface", router)
	}
	h := &markerHandler{}
	if got := fleet.lanHandler(h); got != http.Handler(h) {
		t.Fatalf("lanHandler wrapped the router: %T", got)
	}
}
