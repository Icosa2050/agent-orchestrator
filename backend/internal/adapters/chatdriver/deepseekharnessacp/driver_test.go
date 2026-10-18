package deepseekharnessacp

import (
	"context"
	"reflect"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestConfigureBootsTheACPProfile(t *testing.T) {
	args, env, err := configure(context.Background(), acpdriver.LaunchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--profile", "acp"}) {
		t.Fatalf("args = %#v", args)
	}
	// The profile owns the model route and credentials, so AO adds no overlay.
	if len(env) != 0 {
		t.Fatalf("env = %#v, want none", env)
	}
}

func TestSessionOptionsCarryModelAndMappedEffort(t *testing.T) {
	got := sessionOptions(ports.ChatTurnSettings{
		Model:  `["deepseek-official","deepseek-v4-flash"]`,
		Effort: "xhigh",
	})
	want := []acpdriver.SessionOption{
		{ID: "model", Value: `["deepseek-official","deepseek-v4-flash"]`},
		{ID: "reasoning_effort", Value: "max"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options\nwant: %#v\n got: %#v", want, got)
	}
}

func TestSessionOptionsOmitUnsetSettings(t *testing.T) {
	if got := sessionOptions(ports.ChatTurnSettings{}); got != nil {
		t.Fatalf("options = %#v, want nil", got)
	}
}

// TestSessionOptionsMapAOVocabularyOntoHarnessLevels covers the mapping that
// keeps a project-level effort configured for another harness from breaking a
// DeepSeek Harness session: Harness accepts only off, low, high, and max.
func TestSessionOptionsMapAOVocabularyOntoHarnessLevels(t *testing.T) {
	cases := map[string]string{
		"none":    "off",
		"minimal": "off",
		"off":     "off",
		"low":     "low",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "max",
		"max":     "max",
		// AO values are matched case-insensitively.
		"HIGH": "high",
	}
	for input, want := range cases {
		options := sessionOptions(ports.ChatTurnSettings{Effort: input})
		if len(options) != 1 || options[0].ID != "reasoning_effort" {
			t.Fatalf("effort %q produced %#v", input, options)
		}
		if options[0].Value != want {
			t.Fatalf("effort %q mapped to %q, want %q", input, options[0].Value, want)
		}
	}
}

func TestSessionOptionsPassThroughUnknownEffort(t *testing.T) {
	options := sessionOptions(ports.ChatTurnSettings{Effort: "turbo"})
	if len(options) != 1 || options[0].Value != "turbo" {
		t.Fatalf("options = %#v, want the value passed through", options)
	}
}

func TestValidateTurnSettings(t *testing.T) {
	cases := []struct {
		name    string
		effort  string
		wantErr bool
	}{
		{name: "empty effort defers to Harness", effort: ""},
		{name: "harness level", effort: "high"},
		{name: "mapped AO level", effort: "medium"},
		{name: "unmappable level", effort: "turbo", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTurnSettings(ports.PermissionModeDefault, ports.ChatTurnSettings{Effort: tc.effort})
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateTurnSettingsAcceptsOpaqueModelValues keeps AO out of Harness's
// model vocabulary: a session's model is the JSON array string its own catalog
// advertised, not a provider/model pair.
func TestValidateTurnSettingsAcceptsOpaqueModelValues(t *testing.T) {
	err := validateTurnSettings(ports.PermissionModeDefault, ports.ChatTurnSettings{
		Model: `["deepseek-official","deepseek-v4-pro"]`,
	})
	if err != nil {
		t.Fatalf("err = %v, want the opaque value accepted", err)
	}
}

func TestPermissionPolicy(t *testing.T) {
	edit, deleteKind, move, execute := acpsdk.ToolKindEdit, acpsdk.ToolKindDelete, acpsdk.ToolKindMove, acpsdk.ToolKindExecute
	options := []acpsdk.PermissionOption{
		{OptionId: "reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
		{OptionId: "allow-always", Kind: acpsdk.PermissionOptionKindAllowAlways},
		{OptionId: "allow-once", Kind: acpsdk.PermissionOptionKindAllowOnce},
	}
	cases := []struct {
		name    string
		mode    ports.PermissionMode
		kind    *acpsdk.ToolKind
		options []acpsdk.PermissionOption
		wantID  acpsdk.PermissionOptionId
		handled bool
	}{
		{name: "bypass edit", mode: ports.PermissionModeBypassPermissions, kind: &edit, options: options, wantID: "allow-once", handled: true},
		{name: "bypass execute", mode: ports.PermissionModeBypassPermissions, kind: &execute, options: options, wantID: "allow-once", handled: true},
		{name: "bypass unknown kind", mode: ports.PermissionModeBypassPermissions, options: options, wantID: "allow-once", handled: true},
		{name: "auto execute", mode: ports.PermissionModeAuto, kind: &execute, options: options, wantID: "allow-once", handled: true},
		{name: "auto unknown kind", mode: ports.PermissionModeAuto, options: options, wantID: "allow-once", handled: true},
		{name: "accept edits edit", mode: ports.PermissionModeAcceptEdits, kind: &edit, options: options, wantID: "allow-once", handled: true},
		{name: "accept edits delete", mode: ports.PermissionModeAcceptEdits, kind: &deleteKind, options: options, wantID: "allow-once", handled: true},
		{name: "accept edits move", mode: ports.PermissionModeAcceptEdits, kind: &move, options: options, wantID: "allow-once", handled: true},
		{name: "accept edits execute", mode: ports.PermissionModeAcceptEdits, kind: &execute, options: options},
		{name: "accept edits unknown kind", mode: ports.PermissionModeAcceptEdits, options: options},
		{name: "default edit", mode: ports.PermissionModeDefault, kind: &edit, options: options},
		{name: "default execute", mode: ports.PermissionModeDefault, kind: &execute, options: options},
		{name: "unset mode", kind: &edit, options: options},
		{name: "bypass without allow once", mode: ports.PermissionModeBypassPermissions, options: options[:2]},
		{name: "auto without allow once", mode: ports.PermissionModeAuto, options: options[:2]},
		{name: "accept edits without allow once", mode: ports.PermissionModeAcceptEdits, kind: &edit, options: options[:2]},
		{name: "bypass without options", mode: ports.PermissionModeBypassPermissions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, handled := permissionPolicy(tc.mode, acpsdk.RequestPermissionRequest{
				ToolCall: acpsdk.ToolCallUpdate{Kind: tc.kind},
				Options:  tc.options,
			})
			if id != tc.wantID || handled != tc.handled {
				t.Fatalf("permissionPolicy = (%q, %v), want (%q, %v)", id, handled, tc.wantID, tc.handled)
			}
		})
	}
}

func TestHarnessConstantMatchesTheRegistryKey(t *testing.T) {
	if domain.HarnessDeepSeek != "deepseek-harness" {
		t.Fatalf("harness = %q", domain.HarnessDeepSeek)
	}
}
