package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Fork: fleet seam (FORK.md). Exactly spawn, send and session kill carry the
// fleet-write annotation, and until the fleet write client lands they refuse
// --host and AO_HOST exactly like every other write: exit 2, the read-only
// message, and no request to the host.
func TestForkFleetWriteAnnotationsAreOptInOnly(t *testing.T) {
	root := NewRootCommand(Deps{})
	var marked []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Annotations[remoteAnnotation] == remoteAnnotationFleetWrite {
			marked = append(marked, strings.TrimPrefix(c.CommandPath(), root.Name()+" "))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	sort.Strings(marked)
	if want := []string{"send", "session kill", "spawn"}; strings.Join(marked, ",") != strings.Join(want, ",") {
		t.Fatalf("fleet-write commands = %v, want %v", marked, want)
	}
	if strings.Contains(remoteCapableCommands, "spawn") || strings.Contains(remoteCapableCommands, "send") || strings.Contains(remoteCapableCommands, "kill") {
		t.Fatalf("remote help advertises a write: %q", remoteCapableCommands)
	}
}

func TestForkFleetWriteCommandsStillRefuseRemoteHosts(t *testing.T) {
	cases := [][]string{
		{"spawn", "--project", "remote-proj", "--name", "x", "--prompt", "hi"},
		{"send", "--session", "remote-1", "--message", "hi"},
		{"session", "kill", "remote-1"},
	}
	for _, args := range cases {
		env := newRemoteTestEnv(t)
		host := newFakeRemoteHost(t, "host-r")
		env.add(t, "mini", host)
		before := len(host.seen())

		_, err := env.run(t, append([]string{"--host", "mini"}, args...)...)
		if ExitCode(err) != 2 || !strings.Contains(err.Error(), "cannot run against a remote host (--host=mini): remote access is read-only") {
			t.Fatalf("%v --host: exit %d, err %v", args, ExitCode(err), err)
		}
		t.Setenv("AO_HOST", "mini")
		_, err = env.run(t, args...)
		if ExitCode(err) != 2 || !strings.Contains(err.Error(), "(AO_HOST=mini): remote access is read-only") {
			t.Fatalf("%v with AO_HOST: exit %d, err %v", args, ExitCode(err), err)
		}
		t.Setenv("AO_HOST", "")
		if got := host.seen(); len(got) != before {
			t.Fatalf("%v: a refused write reached the host: %v", args, got[before:])
		}
	}
}

// markRemoteFleetWrite never widens a command already marked local-only.
func TestForkFleetWriteNeverWidensLocalOnly(t *testing.T) {
	cmd := &cobra.Command{Use: "x", Annotations: map[string]string{remoteAnnotation: remoteAnnotationLocalOnly}}
	if got := markRemoteFleetWrite(cmd).Annotations[remoteAnnotation]; got != remoteAnnotationLocalOnly {
		t.Fatalf("annotation = %q, want local-only kept", got)
	}
	if got := markRemoteFleetWrite(&cobra.Command{Use: "y"}).Annotations[remoteAnnotation]; got != remoteAnnotationFleetWrite {
		t.Fatalf("annotation = %q, want fleet-write", got)
	}
}
