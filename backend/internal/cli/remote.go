package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// defaultRemotePort is the authenticated LAN listener's usual port.
const defaultRemotePort = "3011"

// remoteMinAPIVersion is the oldest host contract (GET /api/v1/identity
// apiVersion) this CLI can read.
const remoteMinAPIVersion = 1

// remoteProbeTimeout bounds each step of a reachability probe.
const remoteProbeTimeout = 5 * time.Second

// remoteCommandTimeout bounds one read on a remote host.
const remoteCommandTimeout = 30 * time.Second

// Per-host states reported by `ao remote ls` and the --fleet views.
const (
	remoteStatusOK            = "ok"
	remoteStatusOffline       = "offline"
	remoteStatusNeedsRepair   = "needs re-pair"
	remoteStatusNeedsUpdate   = "needs update"
	remoteStatusHostMismatch  = "host mismatch"
	remoteStatusUntrustedNet  = "untrusted network"
	remoteStatusError         = "error"
	remoteAnnotation          = "ao.remote"
	remoteAnnotationReadOnly  = "read-only"
	remoteAnnotationLocalOnly = "local"
)

// tailscaleNets are the addresses Tailscale assigns: the CGNAT range and its
// IPv6 ULA prefix. Traffic to them rides WireGuard.
var tailscaleNets = func() []*net.IPNet {
	nets := make([]*net.IPNet, 0, 2)
	for _, cidr := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

func trustedRemoteIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, n := range tailscaleNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// normalizeRemoteURL accepts http(s)://host[:port] and fills the LAN
// listener's port for plain http.
func normalizeRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usageError{fmt.Errorf("invalid host URL %q: use http://<host>:3011 or https://<host>", raw)}
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, usageError{fmt.Errorf("invalid host URL %q: give only scheme, host and port", raw)}
	}
	u.Path = ""
	if u.Port() == "" && u.Scheme == "http" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultRemotePort)
	}
	return u, nil
}

// checkRemoteNetwork refuses to send a token anywhere but loopback or
// Tailscale (an address in its ranges, a *.ts.net name, or a MagicDNS name
// that resolves only into them) unless the user allowed that URL explicitly.
func checkRemoteNetwork(ctx context.Context, u *url.URL, allowUntrusted bool) error {
	if allowUntrusted {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return nil
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		lookupCtx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if !trustedRemoteIP(ip) {
			return errUntrustedNetwork{host: u.Host}
		}
	}
	if len(ips) == 0 {
		return errUntrustedNetwork{host: u.Host}
	}
	return nil
}

type errUntrustedNetwork struct{ host string }

func (e errUntrustedNetwork) Error() string {
	return fmt.Sprintf("%s is neither loopback nor a Tailscale address; the CLI sends device tokens only over Tailscale or loopback. "+
		"Use the host's Tailscale address, or pass --allow-untrusted-network to `ao remote add` to allow this URL explicitly", e.host)
}

// remoteTarget is the host a --host command talks to.
type remoteTarget struct {
	remote cliRemote
	token  string

	verifyOnce sync.Once
	verifyErr  error
}

// remoteAuthError means the host no longer accepts this CLI's device token.
type remoteAuthError struct {
	name string
	body apiError
}

func (e remoteAuthError) Error() string {
	return fmt.Sprintf("host %q no longer accepts this CLI's device token (%s). Re-pair: run `ao remote-host pair` on that host, then `ao remote add %s <url> --code <code> --force`",
		e.name, e.body.String(), e.name)
}

// remoteOfflineError means the host could not be reached at all.
type remoteOfflineError struct {
	name, url string
	cause     error
}

func (e remoteOfflineError) Error() string {
	return fmt.Sprintf("host %q is offline or unreachable at %s: %v", e.name, e.url, e.cause)
}

func (e remoteOfflineError) Unwrap() error { return e.cause }

// remoteMismatchError means another machine answered at the saved address.
type remoteMismatchError struct{ name, want, got string }

func (e remoteMismatchError) Error() string {
	return fmt.Sprintf("host %q answered as host %s, not the paired %s; the token was not sent. Check the address, or re-pair with `ao remote add %s <url> --code <code> --force`",
		e.name, e.got, e.want, e.name)
}

// remoteHTTPClient never follows redirects, so a bearer token cannot leak to
// another origin.
func (c *commandContext) remoteHTTPClient(timeout time.Duration) *http.Client {
	client := *c.deps.HTTPClient
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

type remoteIdentity struct {
	HostID     string `json:"hostId"`
	APIVersion int    `json:"apiVersion"`
}

// errIdentityMissing marks a host too old to serve GET /api/v1/identity.
var errIdentityMissing = errors.New("the host does not serve /api/v1/identity")

// fetchRemoteIdentity reads the unauthenticated identity probe; no credential
// is sent.
func (c *commandContext) fetchRemoteIdentity(ctx context.Context, base string) (remoteIdentity, error) {
	reqCtx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/api/v1/identity", http.NoBody)
	if err != nil {
		return remoteIdentity{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.remoteHTTPClient(remoteProbeTimeout).Do(req)
	if err != nil {
		return remoteIdentity{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return remoteIdentity{}, errIdentityMissing
	}
	if resp.StatusCode != http.StatusOK {
		return remoteIdentity{}, fmt.Errorf("identity probe answered HTTP %d", resp.StatusCode)
	}
	var id remoteIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&id); err != nil {
		return remoteIdentity{}, fmt.Errorf("identity probe: %w", err)
	}
	if id.HostID == "" {
		return remoteIdentity{}, errors.New("identity probe returned no host id")
	}
	return id, nil
}

// verify confirms, once per command, that the saved host answers at its
// address before the token is presented.
func (t *remoteTarget) verify(ctx context.Context, c *commandContext) error {
	t.verifyOnce.Do(func() {
		u, err := url.Parse(t.remote.URL)
		if err != nil {
			t.verifyErr = err
			return
		}
		if err := checkRemoteNetwork(ctx, u, t.remote.AllowInsecure); err != nil {
			t.verifyErr = err
			return
		}
		id, err := c.fetchRemoteIdentity(ctx, t.remote.URL)
		switch {
		case errors.Is(err, errIdentityMissing):
			t.verifyErr = fmt.Errorf("host %q needs an update: it does not serve the identity probe", t.remote.Name)
		case err != nil:
			t.verifyErr = remoteOfflineError{name: t.remote.Name, url: t.remote.URL, cause: err}
		case id.HostID != t.remote.HostID:
			t.verifyErr = remoteMismatchError{name: t.remote.Name, want: t.remote.HostID, got: id.HostID}
		case id.APIVersion < remoteMinAPIVersion:
			t.verifyErr = fmt.Errorf("host %q needs an update: API version %d, this CLI needs %d", t.remote.Name, id.APIVersion, remoteMinAPIVersion)
		}
	})
	return t.verifyErr
}

// doRemoteJSON is doJSON against a saved remote host. Remote access is
// read-only: anything but GET is refused before a request is built.
func (c *commandContext) doRemoteJSON(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	if method != http.MethodGet || body != nil {
		return fmt.Errorf("refusing %s %s on remote host %q: --host is read-only", method, path, c.remote.remote.Name)
	}
	if err := c.remote.verify(ctx, c); err != nil {
		return err
	}
	if timeout <= 0 || timeout > remoteCommandTimeout {
		timeout = remoteCommandTimeout
	}
	target := c.remote.remote.URL + path
	req, err := http.NewRequestWithContext(ctx, method, target, http.NoBody) // #nosec G704 -- target is a saved, verified remote host.
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.remote.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.remoteHTTPClient(timeout).Do(req) // #nosec G704 -- see above.
	if err != nil {
		return remoteOfflineError{name: c.remote.remote.Name, url: c.remote.remote.URL, cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		var e apiError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
		return remoteAuthError{name: c.remote.remote.Name, body: e}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("host %q answered with a redirect (HTTP %d); not following it with the token", c.remote.remote.Name, resp.StatusCode)
	}
	return decodeDaemonResponse(resp, method, path, out)
}

// openRemote loads a saved remote and its token.
func (c *commandContext) openRemote(ctx context.Context, name string) (*remoteTarget, error) {
	remote, ok, err := c.findRemote(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, usageError{fmt.Errorf("no saved remote %q; add one with `ao remote add %s <url> --code <code>` (see `ao remote ls`)", name, name)}
	}
	secrets, err := c.remoteSecrets()
	if err != nil {
		return nil, err
	}
	token, err := secrets.Get(ctx, name)
	if errors.Is(err, errRemoteTokenMissing) {
		return nil, fmt.Errorf("no token saved for remote %q; re-pair with `ao remote add %s %s --code <code> --force`", name, name, remote.URL)
	}
	if err != nil {
		return nil, err
	}
	return &remoteTarget{remote: remote, token: token}, nil
}

// withRemote returns a copy of c whose daemon calls go to target.
func (c *commandContext) withRemote(target *remoteTarget) *commandContext {
	return &commandContext{deps: c.deps, remote: target}
}

// remoteHealth is one host's row in `ao remote ls` and the --fleet views.
type remoteHealth struct {
	Name       string `json:"name"`
	Local      bool   `json:"local,omitempty"`
	URL        string `json:"url,omitempty"`
	Status     string `json:"status"`
	HostID     string `json:"hostId,omitempty"`
	APIVersion int    `json:"apiVersion,omitempty"`
	Error      string `json:"error,omitempty"`
}

// probeRemote classifies one saved host without failing the caller.
func (c *commandContext) probeRemote(ctx context.Context, remote cliRemote) (remoteHealth, *remoteTarget) {
	h := remoteHealth{Name: remote.Name, URL: remote.URL, HostID: remote.HostID}
	u, err := url.Parse(remote.URL)
	if err == nil {
		err = checkRemoteNetwork(ctx, u, remote.AllowInsecure)
	}
	if err != nil {
		h.Status, h.Error = remoteStatusUntrustedNet, err.Error()
		return h, nil
	}
	id, err := c.fetchRemoteIdentity(ctx, remote.URL)
	switch {
	case errors.Is(err, errIdentityMissing):
		h.Status, h.Error = remoteStatusNeedsUpdate, err.Error()
		return h, nil
	case err != nil:
		h.Status, h.Error = remoteStatusOffline, err.Error()
		return h, nil
	case id.HostID != remote.HostID:
		h.Status, h.Error = remoteStatusHostMismatch, fmt.Sprintf("answered as host %s", id.HostID)
		return h, nil
	}
	h.APIVersion = id.APIVersion
	if id.APIVersion < remoteMinAPIVersion {
		h.Status, h.Error = remoteStatusNeedsUpdate, fmt.Sprintf("API version %d, this CLI needs %d", id.APIVersion, remoteMinAPIVersion)
		return h, nil
	}
	target, err := c.openRemote(ctx, remote.Name)
	if err != nil {
		h.Status, h.Error = remoteStatusNeedsRepair, err.Error()
		return h, nil
	}
	target.verifyOnce.Do(func() {}) // identity was checked above
	reqCtx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
	defer cancel()
	var projects json.RawMessage
	err = c.withRemote(target).doRemoteJSON(reqCtx, http.MethodGet, "/api/v1/projects", nil, &projects, remoteProbeTimeout)
	var authErr remoteAuthError
	var offline remoteOfflineError
	var apiErr apiResponseError
	switch {
	case err == nil:
		h.Status = remoteStatusOK
		return h, target
	case errors.As(err, &authErr):
		h.Status, h.Error = remoteStatusNeedsRepair, authErr.body.String()
	case errors.As(err, &offline):
		h.Status, h.Error = remoteStatusOffline, offline.cause.Error()
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		h.Status, h.Error = remoteStatusNeedsUpdate, err.Error()
	default:
		h.Status, h.Error = remoteStatusError, err.Error()
	}
	return h, nil
}

// probeRemotes probes every saved host concurrently, keeping their order.
func (c *commandContext) probeRemotes(ctx context.Context, remotes []cliRemote) ([]remoteHealth, []*remoteTarget) {
	health := make([]remoteHealth, len(remotes))
	targets := make([]*remoteTarget, len(remotes))
	var wg sync.WaitGroup
	for i, r := range remotes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			health[i], targets[i] = c.probeRemote(ctx, r)
		}()
	}
	wg.Wait()
	return health, targets
}

func newRemoteCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage remote Orchestrator hosts this CLI can read with its own device token",
		Long: "Pair this CLI with other Orchestrator hosts over Tailscale and read them with --host <name>.\n\n" +
			"Each host issues this CLI its own device token from a one-time code (`ao remote-host pair` on that host).\n" +
			"Tokens live in the macOS login keychain (service " + remoteKeychainService + ") or, elsewhere, in a 0600 file\n" +
			"under ~/.ao; names and addresses live in ~/.ao/cli-remotes.json.",
		Annotations: map[string]string{remoteAnnotation: remoteAnnotationLocalOnly},
	}
	cmd.AddCommand(newRemoteAddCommand(ctx), newRemoteListCommand(ctx), newRemoteRemoveCommand(ctx))
	return cmd
}

type pairDeviceResult struct {
	HostID   string `json:"hostId"`
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
}

func newRemoteAddCommand(ctx *commandContext) *cobra.Command {
	var code string
	var force, allowUntrusted bool
	cmd := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Pair this CLI with a remote host using a one-time code from `ao remote-host pair`",
		Long: "Pair this CLI with a remote host using a one-time code from `ao remote-host pair` on that host.\n\n" +
			"The URL is the host's authenticated listener, usually its Tailscale address, e.g. http://100.101.102.103:3011\n" +
			"or http://mac-mini:3011 (MagicDNS). Plain http without a port defaults to :3011. Device tokens are sent only\n" +
			"to loopback or Tailscale addresses unless --allow-untrusted-network allows this URL explicitly.",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runRemoteAdd(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], code, force, allowUntrusted)
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "One-time pairing code from `ao remote-host pair` on the host (required)")
	cmd.Flags().BoolVar(&force, "force", false, "Replace a saved remote with the same name (re-pair)")
	cmd.Flags().BoolVar(&allowUntrusted, "allow-untrusted-network", false, "Allow sending the token to this URL although it is neither loopback nor Tailscale")
	return cmd
}

func (c *commandContext) runRemoteAdd(ctx context.Context, out io.Writer, name, rawURL, code string, force, allowUntrusted bool) error {
	if err := validRemoteName(name); err != nil {
		return err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return usageError{errors.New("--code is required: run `ao remote-host pair` on the host for a one-time code")}
	}
	u, err := normalizeRemoteURL(rawURL)
	if err != nil {
		return err
	}
	existing, exists, err := c.findRemote(name)
	if err != nil {
		return err
	}
	if exists && !force {
		return fmt.Errorf("a remote named %q already exists (%s); pass --force to re-pair it", name, existing.URL)
	}
	if err := checkRemoteNetwork(ctx, u, allowUntrusted); err != nil {
		return err
	}
	base := u.String()
	id, err := c.fetchRemoteIdentity(ctx, base)
	switch {
	case errors.Is(err, errIdentityMissing):
		return fmt.Errorf("%s does not look like an Orchestrator host that supports device pairing; update it", base)
	case err != nil:
		return fmt.Errorf("reach %s: %w", base, err)
	}

	hostname, _ := os.Hostname()
	if hostname = strings.TrimSpace(hostname); hostname == "" {
		hostname = "Unknown host"
	}
	payload, err := json.Marshal(map[string]string{"name": fitHostLabel(hostname + " CLI"), "platform": runtime.GOOS})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, remoteCommandTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/api/v1/remote-host/devices", bytes.NewReader(payload)) // #nosec G704 -- the user named this host explicitly.
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+code)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.remoteHTTPClient(remoteCommandTimeout).Do(req) // #nosec G704 -- see above.
	if err != nil {
		return fmt.Errorf("pair with %s: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var e apiError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("the host refused the pairing code (%s); codes work once and expire, so run `ao remote-host pair` on the host for a new one", e.String())
		case http.StatusNotFound, http.StatusNotImplemented:
			return fmt.Errorf("the host does not support device pairing (HTTP %d); update it", resp.StatusCode)
		default:
			return fmt.Errorf("pair with %s: %w", base, apiResponseError{StatusCode: resp.StatusCode, ErrorBody: e})
		}
	}
	var paired pairDeviceResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&paired); err != nil {
		return fmt.Errorf("pair with %s: decode response: %w", base, err)
	}
	if paired.HostID != id.HostID {
		return fmt.Errorf("pair with %s: the pairing answer named host %s, but the host identified as %s", base, paired.HostID, id.HostID)
	}
	if !deviceTokenPattern.MatchString(paired.Token) {
		return fmt.Errorf("pair with %s: the host returned a malformed token", base)
	}

	secrets, err := c.remoteSecrets()
	if err != nil {
		return err
	}
	if err := secrets.Set(ctx, name, paired.Token); err != nil {
		return err
	}
	remotes, err := c.loadRemotes()
	if err != nil {
		return err
	}
	kept := remotes[:0]
	for _, r := range remotes {
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	kept = append(kept, cliRemote{
		Name: name, URL: base, HostID: paired.HostID, DeviceID: paired.DeviceID,
		AllowInsecure: allowUntrusted, AddedAt: c.deps.Now().UTC(),
	})
	if err := c.saveRemotes(kept); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Paired %q with host %s at %s as device %s.\nToken saved in %s.\n",
		name, paired.HostID, base, paired.DeviceID, secrets.Describe()); err != nil {
		return err
	}
	if exists && existing.DeviceID != "" && existing.DeviceID != paired.DeviceID {
		_, err = fmt.Fprintf(out, "The previous device %s may still be listed on that host; revoke it there with `ao remote-host devices revoke %s`.\n", existing.DeviceID, existing.DeviceID)
	}
	return err
}

func newRemoteListCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List saved remote hosts with their reachability",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			remotes, err := ctx.loadRemotes()
			if err != nil {
				return err
			}
			health, _ := ctx.probeRemotes(cmd.Context(), remotes)
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"remotes": health})
			}
			if len(health) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No remotes. Add one with `ao remote add <name> <url> --code <code>`.")
				return err
			}
			return writeHostHealth(cmd.OutOrStdout(), health)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}

func writeHostHealth(out io.Writer, health []remoteHealth) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tURL\tSTATUS\tHOST ID\tAPI\tDETAIL"); err != nil {
		return err
	}
	for _, h := range health {
		api := "-"
		if h.APIVersion > 0 {
			api = fmt.Sprintf("v%d", h.APIVersion)
		}
		where := h.URL
		if h.Local {
			where = "(this machine)"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, where, h.Status, orDash(h.HostID), api, orDash(h.Error)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newRemoteRemoveCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Forget a saved remote and delete its local token",
		Long: "Forget a saved remote and delete its local token.\n\n" +
			"A device cannot manage the host's device list over the network, so the token stays valid on the host until\n" +
			"you revoke it there with `ao remote-host devices revoke <device-id>`; the command prints the ID.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			remotes, err := ctx.loadRemotes()
			if err != nil {
				return err
			}
			var target *cliRemote
			kept := make([]cliRemote, 0, len(remotes))
			for i := range remotes {
				if remotes[i].Name == name {
					target = &remotes[i]
					continue
				}
				kept = append(kept, remotes[i])
			}
			if target == nil {
				return fmt.Errorf("no saved remote %q; see `ao remote ls`", name)
			}
			secrets, err := ctx.remoteSecrets()
			if err != nil {
				return err
			}
			if err := secrets.Delete(cmd.Context(), name); err != nil {
				return err
			}
			if err := ctx.saveRemotes(kept); err != nil {
				return err
			}
			message := fmt.Sprintf("Removed %q and deleted its local token.\n", name)
			if target.DeviceID != "" {
				message += fmt.Sprintf("The token stays valid on host %s until you revoke it there: `ao remote-host devices revoke %s`.\n", target.HostID, target.DeviceID)
			}
			_, err = io.WriteString(cmd.OutOrStdout(), message)
			return err
		},
	}
}
