// Package kimiacp binds the user's own Kimi Code installation to AO's
// reusable ACP Chat transport.
package kimiacp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/kimi"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// New launches `kimi acp` from the exact binary resolved by the existing Kimi
// agent plugin. Authentication, model discovery, sessions, and configuration
// remain owned by that installation.
func New(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness: domain.HarnessKimi,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityHistory: true,
			ports.ChatCapabilityPlans:   true,
		},
		Configure:            configure,
		SessionMode:          sessionMode,
		SessionOptions:       sessionOptions,
		ValidateTurnSettings: validateTurnSettings,
	}, log)
}

func configure(ctx context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	if err := validateTurnSettings(cfg.Permissions, ports.ChatTurnSettings{Approval: cfg.Permissions}); err != nil {
		return nil, nil, err
	}
	if err := kimi.PrepareACPInstructions(ctx, cfg.WorkspacePath, cfg.SystemPrompt); err != nil {
		return nil, nil, err
	}
	return []string{"acp"}, nil, nil
}

// validateTurnSettings rejects the one AO permission mode Kimi cannot honor:
// none of its session modes approves edits while still asking for commands.
func validateTurnSettings(_ ports.PermissionMode, settings ports.ChatTurnSettings) error {
	if mode := ports.NormalizePermissionMode(settings.Approval); mode == ports.PermissionModeAcceptEdits {
		return fmt.Errorf("%w: Kimi ACP has no session mode that approves edits only; requested %q",
			ports.ErrChatPermissionModeUnsupported, mode)
	}
	return nil
}

// sessionMode maps AO's permission vocabulary onto Kimi Code's ACP mode ids
// (default, plan, auto, yolo), advertised on every session/new.
//
// The ids read the opposite of what they do. Over ACP `auto` never asks, not
// even before a destructive command, while `yolo` runs routine edits and
// commands but still asks before risky ones (measured against Kimi Code 2.1.1;
// MoonshotAI/kimi-code#1874 tracks the inverted descriptions). So AO's bypass
// selects `auto` and AO's auto selects `yolo`. An unset mode leaves Kimi's own
// session default unchanged.
func sessionMode(permission ports.PermissionMode) string {
	switch permission {
	case ports.PermissionModeDefault:
		return "default"
	case ports.PermissionModeAuto:
		return "yolo"
	case ports.PermissionModeBypassPermissions:
		return "auto"
	default:
		return ""
	}
}

// sessionOptions maps AO's durable model choice onto Kimi's advertised legacy
// model selector. The generic ACP transport routes it through session/set_model.
func sessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	var options []acpdriver.SessionOption
	if settings.Model != "" {
		options = append(options, acpdriver.SessionOption{ID: "model", Value: settings.Model})
	}
	return options
}
