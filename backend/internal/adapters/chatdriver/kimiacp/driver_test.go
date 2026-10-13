package kimiacp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakePlugin struct {
	binary string
	status ports.AgentAuthStatus
}

func (p fakePlugin) ResolveBinary(context.Context) (string, error) { return p.binary, nil }
func (p fakePlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return p.status, nil
}

func TestDriverReusesKimiPluginAndDeclaresNativeFeatures(t *testing.T) {
	driver := New(fakePlugin{binary: "/user/bin/kimi", status: ports.AgentAuthStatusAuthorized}, nil)
	if driver.Harness() != domain.HarnessKimi {
		t.Fatalf("harness = %q, want %q", driver.Harness(), domain.HarnessKimi)
	}
	caps, err := driver.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, capability := range []ports.ChatCapability{
		ports.ChatCapabilityStreaming,
		ports.ChatCapabilityTools,
		ports.ChatCapabilityApprovals,
		ports.ChatCapabilityInterrupt,
		ports.ChatCapabilityResume,
		ports.ChatCapabilityHistory,
		ports.ChatCapabilityPlans,
	} {
		if !caps.Has(capability) {
			t.Errorf("capability %q is false", capability)
		}
	}
}

func TestConfigureLaunchesNativeACPSubcommand(t *testing.T) {
	workspace := t.TempDir()
	args, env, err := configure(context.Background(), acpdriver.LaunchConfig{
		WorkspacePath: workspace,
		Model:         "kimi-code/kimi-for-coding", Permissions: ports.PermissionModeDefault,
		SystemPrompt: "AO worker instructions",
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if want := []string{"acp"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	if env != nil {
		t.Fatalf("env = %#v, want nil", env)
	}
	instructions, err := os.ReadFile(filepath.Join(workspace, ".kimi-code", "AGENTS.md"))
	if err != nil {
		t.Fatalf("read Kimi ACP instructions: %v", err)
	}
	for _, want := range []string{
		"<!-- managed by agent-orchestrator: kimi system prompt -->",
		"AO worker instructions",
		"<!-- /managed by agent-orchestrator: kimi system prompt -->",
	} {
		if !strings.Contains(string(instructions), want) {
			t.Errorf("Kimi ACP instructions missing %q:\n%s", want, instructions)
		}
	}
	gitignore, err := os.ReadFile(filepath.Join(workspace, ".kimi-code", ".gitignore"))
	if err != nil {
		t.Fatalf("read Kimi ACP gitignore: %v", err)
	}
	if !strings.Contains(string(gitignore), "/AGENTS.md\n") {
		t.Fatalf("Kimi ACP instructions are not gitignored:\n%s", gitignore)
	}
}

func TestConfigureRejectsOnlyAcceptEdits(t *testing.T) {
	for _, mode := range []ports.PermissionMode{
		"",
		ports.PermissionModeDefault,
		ports.PermissionModeAuto,
		ports.PermissionModeBypassPermissions,
	} {
		t.Run("accepts "+string(mode), func(t *testing.T) {
			if _, _, err := configure(context.Background(), acpdriver.LaunchConfig{
				WorkspacePath: t.TempDir(), Permissions: mode,
			}); err != nil {
				t.Fatalf("configure permissions %q: %v", mode, err)
			}
		})
	}
	_, _, err := configure(context.Background(), acpdriver.LaunchConfig{
		WorkspacePath: t.TempDir(), Permissions: ports.PermissionModeAcceptEdits,
	})
	if !errors.Is(err, ports.ErrChatPermissionModeUnsupported) {
		t.Fatalf("configure accept-edits error = %v, want typed unsupported-mode error", err)
	}
}

// Kimi's `auto` never asks and its `yolo` still asks before risky commands, so
// the mapping deliberately crosses the names.
func TestSessionModeMapsPermissionsOntoKimiModeIDs(t *testing.T) {
	for permission, want := range map[ports.PermissionMode]string{
		"":                                    "",
		ports.PermissionModeDefault:           "default",
		ports.PermissionModeAcceptEdits:       "",
		ports.PermissionModeAuto:              "yolo",
		ports.PermissionModeBypassPermissions: "auto",
	} {
		if got := sessionMode(permission); got != want {
			t.Errorf("sessionMode(%q) = %q, want %q", permission, got, want)
		}
	}
}

func TestSessionOptionsMapModelsButDoNotInventPermissionModes(t *testing.T) {
	tests := []struct {
		name     string
		settings ports.ChatTurnSettings
		want     []acpdriver.SessionOption
	}{
		{name: "empty"},
		{
			name:     "model",
			settings: ports.ChatTurnSettings{Model: "kimi-code/kimi-for-coding"},
			want:     []acpdriver.SessionOption{{ID: "model", Value: "kimi-code/kimi-for-coding"}},
		},
		{
			name:     "accept edits",
			settings: ports.ChatTurnSettings{Approval: ports.PermissionModeAcceptEdits},
		},
		{
			name:     "auto",
			settings: ports.ChatTurnSettings{Approval: ports.PermissionModeAuto},
		},
		{
			name:     "bypass",
			settings: ports.ChatTurnSettings{Approval: ports.PermissionModeBypassPermissions},
		},
		{
			name: "model and permission",
			settings: ports.ChatTurnSettings{
				Model: "kimi-code/kimi-for-coding", Approval: ports.PermissionModeBypassPermissions,
			},
			want: []acpdriver.SessionOption{
				{ID: "model", Value: "kimi-code/kimi-for-coding"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionOptions(tc.settings); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sessionOptions(%#v) = %#v, want %#v", tc.settings, got, tc.want)
			}
		})
	}
}

func TestLiveKimiACPAcceptsMappedSessionModes(t *testing.T) {
	if os.Getenv("AO_KIMI_ACP_INTEGRATION") != "1" {
		t.Skip("set AO_KIMI_ACP_INTEGRATION=1 to run the live Kimi ACP session-mode contract")
	}
	bin, err := exec.LookPath("kimi")
	if err != nil {
		t.Fatalf("find kimi: %v", err)
	}
	driver := New(fakePlugin{binary: bin, status: ports.AgentAuthStatusAuthorized},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, mode := range []ports.PermissionMode{
		ports.PermissionModeDefault,
		ports.PermissionModeAuto,
		ports.PermissionModeBypassPermissions,
	} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			conv, err := driver.Start(ctx, ports.ChatStartConfig{
				SessionID: domain.SessionID("live-kimi-acp-mode-" + string(mode)), DataDir: t.TempDir(),
				WorkspacePath: t.TempDir(), Env: envMap(), Permissions: mode,
			})
			if err != nil {
				t.Fatalf("start live Kimi ACP with %q: %v", mode, err)
			}
			conv.Close()
		})
	}
}

func envMap() map[string]string {
	out := make(map[string]string)
	for _, pair := range os.Environ() {
		if name, value, ok := strings.Cut(pair, "="); ok {
			out[name] = value
		}
	}
	return out
}

func TestLiveKimiACPReceivesLaunchSystemPrompt(t *testing.T) {
	if os.Getenv("AO_KIMI_ACP_INTEGRATION") != "1" {
		t.Skip("set AO_KIMI_ACP_INTEGRATION=1 to run the live Kimi ACP prompt-delivery contract")
	}
	bin, err := exec.LookPath("kimi")
	if err != nil {
		t.Fatalf("find kimi: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	driver := New(fakePlugin{binary: bin, status: ports.AgentAuthStatusAuthorized},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	conv, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "live-kimi-acp-prompt", DataDir: t.TempDir(),
		WorkspacePath: t.TempDir(), Env: envMap(),
		SystemPrompt: "When the user sends AO_KIMI_PROMPT_HANDSHAKE, reply with exactly " +
			"AO_KIMI_PROMPT_RECEIVED and no other text.",
	})
	if err != nil {
		t.Fatalf("start live Kimi ACP: %v", err)
	}
	defer conv.Close()

	for {
		select {
		case event := <-conv.Events():
			if event.Kind == ports.ChatEventControllerState && event.ControllerState == ports.ChatControllerReady {
				goto ready
			}
		case <-ctx.Done():
			t.Fatalf("wait for Kimi ready: %v", ctx.Err())
		}
	}

ready:
	ref, err := conv.SendTurn(ctx, ports.ChatUserMessage{Text: "AO_KIMI_PROMPT_HANDSHAKE"})
	if err != nil {
		t.Fatalf("send Kimi handshake: %v", err)
	}
	if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("start Kimi handshake: %v", err)
	}

	answer := ""
	for {
		select {
		case event := <-conv.Events():
			if event.Kind == ports.ChatEventMessageCompleted {
				answer = event.Text
			}
			if event.Kind == ports.ChatEventTurnCompleted {
				if strings.TrimSpace(answer) != "AO_KIMI_PROMPT_RECEIVED" {
					t.Fatalf("Kimi response = %q, want system-prompt handshake", answer)
				}
				return
			}
		case <-ctx.Done():
			t.Fatalf("wait for Kimi handshake: %v", ctx.Err())
		}
	}
}
