package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// remoteCapableCommands documents what --host accepts; each command opts in
// with the remoteAnnotation set to read-only.
const remoteCapableCommands = "status, session ls, session get, project ls, project get, account ls, usage report, usage limits"

// markRemoteReadOnly opts cmd into --host. Only commands that issue GETs may
// opt in; the remote transport refuses any other method regardless.
func markRemoteReadOnly(cmd *cobra.Command) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[remoteAnnotation] = remoteAnnotationReadOnly
	return cmd
}

// markRemoteFleetWrite marks cmd as a fleet-control write (spawn, send,
// session kill). Fork: fleet seam (FORK.md). selectRemoteHost still refuses
// such commands against a remote host; the fleet write client opts them in
// later, with an explicit --host only.
func markRemoteFleetWrite(cmd *cobra.Command) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	if cmd.Annotations[remoteAnnotation] == remoteAnnotationLocalOnly {
		return cmd // never widen a local-only command
	}
	cmd.Annotations[remoteAnnotation] = remoteAnnotationFleetWrite
	return cmd
}

func remoteMode(cmd *cobra.Command) (mode string, hidden bool) {
	for c := cmd; c != nil; c = c.Parent() {
		hidden = hidden || c.Hidden
		if mode == "" {
			mode = c.Annotations[remoteAnnotation]
		}
	}
	return mode, hidden
}

// selectRemoteHost points c at the host named by --host or AO_HOST. A command
// that has not opted in is refused, so a mutation never lands on the wrong
// machine. Internal hidden commands and agent sessions (AO_SESSION_ID) ignore
// an inherited AO_HOST.
func (c *commandContext) selectRemoteHost(cmd *cobra.Command, flagValue string) error {
	fromFlag := false
	if f := cmd.Flag("host"); f != nil && f.Changed {
		fromFlag = true
	}
	name := strings.TrimSpace(flagValue)
	if !fromFlag {
		// An AO-managed agent session inherits the user's shell environment;
		// its commands must keep reaching its own daemon. Only an explicit
		// --host sends them elsewhere.
		if strings.TrimSpace(os.Getenv("AO_SESSION_ID")) != "" {
			return nil
		}
		name = strings.TrimSpace(os.Getenv("AO_HOST"))
	}
	if name == "" {
		return nil
	}
	source := "--host"
	if !fromFlag {
		source = "AO_HOST"
	}
	mode, hidden := remoteMode(cmd)
	switch {
	case mode == remoteAnnotationLocalOnly || cmd.Name() == "help" || cmd.Name() == "version" || cmd.Name() == "completion":
		return nil
	case mode == remoteAnnotationReadOnly:
	case !fromFlag && hidden:
		return nil
	default:
		return usageError{fmt.Errorf("`%s` cannot run against a remote host (%s=%s): remote access is read-only and supports only: %s",
			cmd.CommandPath(), source, name, remoteCapableCommands)}
	}
	if f := cmd.Flag("fleet"); f != nil && f.Changed {
		return usageError{fmt.Errorf("--fleet reads every saved host; do not combine it with %s", source)}
	}
	if err := validRemoteName(name); err != nil {
		return err
	}
	target, err := c.openRemote(cmd.Context(), name)
	if err != nil {
		return err
	}
	c.remote = target
	return nil
}

type fleetUsageHost struct {
	remoteHealth
	Data json.RawMessage `json:"data,omitempty"`
}

// fleetGetJSON reads one GET route from this machine and every saved remote.
// A host that cannot answer keeps its status and error instead of failing the run.
func (c *commandContext) fleetGetJSON(ctx context.Context, path string) ([]fleetUsageHost, error) {
	health, contexts, err := c.fleetHosts(ctx)
	if err != nil {
		return nil, err
	}
	hosts := make([]fleetUsageHost, len(health))
	var wg sync.WaitGroup
	for i := range health {
		hosts[i] = fleetUsageHost{remoteHealth: health[i]}
		if contexts[i] == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var data json.RawMessage
			if err := contexts[i].getJSON(ctx, path, &data); err != nil {
				hosts[i].Status, hosts[i].Error = fleetErrorStatus(err), err.Error()
				return
			}
			hosts[i].Data = data
		}()
	}
	wg.Wait()
	return hosts, nil
}

// localHealth maps the loopback daemon's state onto the fleet vocabulary.
func (c *commandContext) localHealth(ctx context.Context) remoteHealth {
	h := remoteHealth{Name: "local", Local: true}
	st, err := c.inspectDaemon(ctx)
	switch {
	case err != nil:
		h.Status, h.Error = remoteStatusError, err.Error()
	case st.State == stateReady:
		h.Status = remoteStatusOK
	case st.State == stateStopped || st.State == stateStale:
		h.Status, h.Error = remoteStatusOffline, "daemon "+string(st.State)
	default:
		h.Status, h.Error = remoteStatusError, strings.TrimSpace("daemon "+string(st.State)+" "+st.Error)
	}
	return h
}

// fleetHosts probes this machine and every saved remote.
func (c *commandContext) fleetHosts(ctx context.Context) ([]remoteHealth, []*commandContext, error) {
	remotes, err := c.loadRemotes()
	if err != nil {
		return nil, nil, err
	}
	health := []remoteHealth{{}}
	contexts := []*commandContext{nil}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		health[0] = c.localHealth(ctx)
	}()
	remoteHealths, targets := c.probeRemotes(ctx, remotes)
	wg.Wait()
	if health[0].Status == remoteStatusOK {
		contexts[0] = &commandContext{deps: c.deps}
	}
	for i := range remoteHealths {
		health = append(health, remoteHealths[i])
		if targets[i] != nil {
			contexts = append(contexts, c.withRemote(targets[i]))
		} else {
			contexts = append(contexts, nil)
		}
	}
	return health, contexts, nil
}

func (c *commandContext) runFleetStatus(cmd *cobra.Command, asJSON bool) error {
	health, _, err := c.fleetHosts(cmd.Context())
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"hosts": health})
	}
	return writeHostHealth(cmd.OutOrStdout(), health)
}

func (c *commandContext) runRemoteStatus(cmd *cobra.Command, asJSON bool) error {
	health, _ := c.probeRemote(cmd.Context(), c.remote.remote)
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), health)
	}
	return writeHostHealth(cmd.OutOrStdout(), []remoteHealth{health})
}

type fleetSessionHost struct {
	remoteHealth
	Data []sessionListEntry `json:"data"`
	Meta *sessionListMeta   `json:"meta,omitempty"`
}

func (c *commandContext) runFleetSessions(cmd *cobra.Command, opts sessionListOptions) error {
	ctx := cmd.Context()
	health, contexts, err := c.fleetHosts(ctx)
	if err != nil {
		return err
	}
	hosts := make([]fleetSessionHost, len(health))
	results := make([]sessionListResult, len(health))
	var wg sync.WaitGroup
	for i := range health {
		hosts[i] = fleetSessionHost{remoteHealth: health[i], Data: []sessionListEntry{}}
		if contexts[i] == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := contexts[i].collectSessions(ctx, opts)
			if err != nil {
				hosts[i].Status, hosts[i].Error = fleetErrorStatus(err), err.Error()
				return
			}
			results[i] = res
			hosts[i].Data = sessionListEntries(res.sessions, res.prSummaries)
			hosts[i].Meta = &sessionListMeta{HiddenTerminatedCount: res.hiddenTerminated, HiddenOrchestratorCount: res.hiddenOrchestrators}
		}()
	}
	wg.Wait()
	if opts.json {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"hosts": hosts})
	}
	out := cmd.OutOrStdout()
	for i, h := range hosts {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		label := h.Name
		if !h.Local {
			label += " " + h.URL
		}
		if _, err := fmt.Fprintf(out, "== %s: %s ==\n", label, h.Status); err != nil {
			return err
		}
		if h.Status != remoteStatusOK {
			if _, err := fmt.Fprintf(out, "  %s\n", orDash(h.Error)); err != nil {
				return err
			}
			continue
		}
		r := results[i]
		if err := writeSessionList(cmd, r.sessions, r.prSummaries, r.hiddenTerminated, r.hiddenOrchestrators, c.deps.Now()); err != nil {
			return err
		}
	}
	return nil
}

func fleetErrorStatus(err error) string {
	var authErr remoteAuthError
	var offline remoteOfflineError
	switch {
	case errors.As(err, &authErr):
		return remoteStatusNeedsRepair
	case errors.As(err, &offline), errors.Is(err, errDaemonUnavailable):
		return remoteStatusOffline
	default:
		return remoteStatusError
	}
}
