package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// appBundleName is the macOS bundle directory name produced by electron-forge
// (spaced, per frontend/forge.config.ts).
const appBundleName = "Icosa Orchestrator.app"

// appStateFileName is the marker the desktop app writes under ~/.ao on every
// launch (spec §5). `ao start` is a read-only consumer of it.
const appStateFileName = "icosa-app-state.json"

// appState mirrors the app-written ~/.ao/app-state.json marker (spec §5). Only
// the desktop app writes it; `ao start` reads it as a fast-path hint and never
// trusts appPath without stat-ing it (invariant 2).
type appState struct {
	SchemaVersion    int    `json:"schemaVersion"`
	AppPath          string `json:"appPath"`
	Version          string `json:"version"`
	InstalledAt      string `json:"installedAt"`
	LastReconciledAt string `json:"lastReconciledAt"`
	InstallSource    string `json:"installSource"`
}

type startOptions struct {
	json bool
}

// startResult is the JSON shape emitted with --json: what `ao start` resolved,
// whether it fetched, whether it opened, and the resulting bundle path.
type startResult struct {
	Resolved bool   `json:"resolved"`
	Fetched  bool   `json:"fetched"`
	Opened   bool   `json:"opened"`
	AppPath  string `json:"appPath"`
}

func newStartCommand(ctx *commandContext) *cobra.Command {
	opts := startOptions{}
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Open the installed Icosa Orchestrator desktop app",
		Long:  "Open the installed Icosa Orchestrator desktop app. Automatic acquisition is disabled for this local build.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.runStart(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output start result as JSON")
	return cmd
}

// runStart opens an existing local install. Missing installs fail without
// acquiring a release. It never blocks or supervises the launched app.
func (c *commandContext) runStart(ctx context.Context, cmd *cobra.Command, opts startOptions) error {
	out := cmd.OutOrStdout()
	res := startResult{}

	appPath := c.resolveApp()
	res.Resolved = appPath != ""

	if appPath == "" {
		return fmt.Errorf("local Icosa Orchestrator app is not installed; automatic acquisition is disabled for this local build")
	}
	res.AppPath = appPath

	if runtime.GOOS == "linux" {
		if err := c.registerLinuxProtocolHandler(ctx, appPath); err != nil {
			return err
		}
	}

	opened, err := c.openApp(ctx, appPath)
	if err != nil {
		return err
	}
	res.Opened = opened

	if opts.json {
		return writeJSON(out, res)
	}

	c.printDeprecationNotice(out)
	if !opened {
		c.printManualOpen(out, appPath)
	}
	return nil
}

// resolveApp returns the path to a usable desktop bundle, or "" when none is
// found (spec §6.2). Resolution order is marker path -> stat -> known location
// scan, with one exception: on macOS a real bundle in /Applications outranks
// the marker. It still never compares versions (invariant 5).
//
// The exception exists because the marker is not authoritative about which copy
// the user wants. The app rewrites it on every launch, so a stale copy (the
// original download, one on a dmg) that got launched leaves the marker pointing
// at itself, and `ao start` would then keep reopening that copy instead of the
// install. /Applications is the one location the updater maintains, so prefer
// it and let the marker cover the rest (~/Applications, unusual layouts).
func (c *commandContext) resolveApp() string {
	if p := preferredAppPath(); p != "" && isUsableBundle(p) {
		return p
	}
	if p := c.markerAppPath(); p != "" && isUsableBundle(p) {
		return p
	}
	for _, p := range appScanLocations() {
		if isUsableBundle(p) {
			return p
		}
	}
	return ""
}

// preferredAppPath is the location that outranks the marker, or "" when the
// platform has none. A package var for the same reason as appScanLocations:
// tests point it at a temp bundle instead of the real system path.
var preferredAppPath = darwinApplicationsBundle

// darwinApplicationsBundle is /Applications/<bundle> on macOS, "" elsewhere.
// That is where Squirrel installs updates, so it is the copy actually kept
// current. Windows and Linux have no equivalent single maintained location, so
// they keep the original marker-first order.
func darwinApplicationsBundle() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	// Concatenated, not filepath.Join'd, to match knownAppLocations (and to keep
	// gocritic's filepathJoin check happy about the leading separator).
	return "/Applications/" + appBundleName
}

// appScanLocations is the known-location scan source. It is a package var so
// tests can point the scan at a temp bundle instead of real system paths.
var appScanLocations = knownAppLocations

// markerAppPath reads ~/.ao/app-state.json and returns its recorded appPath, or
// "" if the marker is missing/unreadable. It does not stat the path; callers do.
func (c *commandContext) markerAppPath() string {
	return c.markerAppPathForOS(runtime.GOOS)
}

func (c *commandContext) markerAppPathForOS(platform string) string {
	dir, err := aoStateDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, appStateFileName))
	if err != nil {
		return ""
	}
	var st appState
	if err := json.Unmarshal(data, &st); err != nil {
		return ""
	}
	if name := forkAppFilename(platform); name == "" || filepath.Base(st.AppPath) != name {
		return ""
	}
	return st.AppPath
}

// aoStateDir resolves the canonical ~/.ao home, honoring AO_DATA_DIR exactly as
// the daemon's config does (the marker lives beside running.json under ~/.ao).
func aoStateDir() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	// running.json lives directly under ~/.ao; the marker sits beside it.
	return filepath.Dir(cfg.RunFilePath), nil
}

// knownAppLocations lists the platform's standard install paths to scan when the
// marker misses (covers website installs and stale markers, spec §6.2).
func knownAppLocations() []string {
	return knownAppLocationsForOS(runtime.GOOS)
}

func knownAppLocationsForOS(platform string) []string {
	switch platform {
	case "darwin":
		paths := []string{"/Applications/" + appBundleName}
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, "Applications", appBundleName))
		}
		return paths
	case "windows":
		var paths []string
		// Default electron-builder NSIS per-user install (perMachine:false).
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			paths = append(paths, windowsInstalledExe(local))
		}
		// Per-machine fallback (if a user chose an all-users install).
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			paths = append(paths, filepath.Join(pf, "Icosa Orchestrator", forkAppFilename("windows")))
		}
		return paths
	case "linux":
		paths := []string{linuxAppImagePath()}
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, "Applications", forkAppFilename("linux")))
		}
		return paths
	default:
		return nil
	}
}

// windowsInstalledExe is the default per-user electron-builder NSIS install
// target for the app exe under %LOCALAPPDATA%.
func windowsInstalledExe(localAppData string) string {
	return filepath.Join(localAppData, "Programs", "Icosa Orchestrator", forkAppFilename("windows"))
}

// forkAppFilename is the identity shared by marker validation and install scans.
func forkAppFilename(platform string) string {
	switch platform {
	case "darwin":
		return appBundleName
	case "windows":
		return "icosa-orchestrator.exe"
	case "linux":
		return "icosa-orchestrator.AppImage"
	default:
		return ""
	}
}

// linuxAppImagePath is the local-distribution AppImage location beside running.json.
func linuxAppImagePath() string {
	dir, err := aoStateDir()
	if err != nil {
		return forkAppFilename("linux")
	}
	return filepath.Join(dir, forkAppFilename("linux"))
}

const linuxDesktopEntryName = "icosa-orchestrator.desktop"

func linuxApplicationsDir() (string, error) {
	// A URL-scheme handler is OS integration metadata, not AO runtime state.
	// freedesktop.org requires desktop entries under XDG_DATA_HOME; keeping the
	// executable and all mutable AO data under ~/.ao is unchanged.
	if dataHome := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dataHome) {
		return filepath.Join(dataHome, "applications"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for Linux protocol registration: %w", err)
	}
	return filepath.Join(home, ".local", "share", "applications"), nil
}

func desktopExecPath(appPath string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"`", "\\`",
		"$", `\$`,
		"%", "%%",
	).Replace(appPath)
	return `"` + escaped + `"`
}

func (c *commandContext) registerLinuxProtocolHandler(
	ctx context.Context,
	appPath string,
) error {
	applicationsDir, err := linuxApplicationsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(applicationsDir, 0o750); err != nil {
		return fmt.Errorf("create Linux applications directory: %w", err)
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Orchestrator
Exec=%s %%u
Terminal=false
NoDisplay=true
MimeType=x-scheme-handler/icosa-orchestrator;
`, desktopExecPath(appPath))
	target := filepath.Join(applicationsDir, linuxDesktopEntryName)
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, []byte(entry), 0o600); err != nil {
		return fmt.Errorf("write Linux desktop entry: %w", err)
	}
	if err := os.Chmod(temporary, 0o644); err != nil { //nolint:gosec // G302: desktop entries must be world-readable
		_ = os.Remove(temporary)
		return fmt.Errorf("chmod Linux desktop entry: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("install Linux desktop entry: %w", err)
	}
	if out, err := c.deps.CommandOutput(
		ctx,
		"xdg-mime",
		"default",
		linuxDesktopEntryName,
		"x-scheme-handler/icosa-orchestrator",
	); err != nil {
		return fmt.Errorf(
			"register icosa-orchestrator URL handler with xdg-mime (install xdg-utils or use the .deb/.rpm package): %w: %s",
			err,
			out,
		)
	}
	out, err := c.deps.CommandOutput(
		ctx,
		"xdg-mime",
		"query",
		"default",
		"x-scheme-handler/icosa-orchestrator",
	)
	if err != nil {
		return fmt.Errorf("verify icosa-orchestrator URL handler: %w: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != linuxDesktopEntryName {
		return fmt.Errorf(
			"verify icosa-orchestrator URL handler: xdg-mime selected %q instead of %q",
			strings.TrimSpace(string(out)),
			linuxDesktopEntryName,
		)
	}
	return nil
}

// isUsableBundle reports whether p stats as a usable app bundle. On macOS a
// bundle is a directory; on Windows/Linux it is a regular file (the installed
// exe / the AppImage). The filesystem is the source of truth (invariant 2).
func isUsableBundle(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	if runtime.GOOS == "darwin" {
		return info.IsDir()
	}
	// Windows exe / Linux AppImage: must be a regular file, not a directory.
	return info.Mode().IsRegular()
}

// openApp launches the resolved bundle detached and reports whether it launched
// (spec §6.5). It passes --installed-via=npm-bootstrap so the app can record the
// install source in its marker. It never waits on the app. A launch failure
// returns (false, nil) so the caller falls back to manual-open instructions.
func (c *commandContext) openApp(ctx context.Context, appPath string) (bool, error) {
	switch runtime.GOOS {
	case "darwin":
		// `open` returns immediately; --args forwards the rest to the app.
		if out, err := c.deps.CommandOutput(ctx, "open", appPath, "--args", "--installed-via=npm-bootstrap"); err != nil {
			return false, fmt.Errorf("open %s: %w: %s", appPath, err, out)
		}
		return true, nil
	case "windows", "linux":
		// No `open`-style launcher on these platforms; exec the bundle directly,
		// detached, so `ao start` does not block on the app. StartProcess uses
		// cmd.Start() + a detached SysProcAttr (see process.go).
		//
		// ponytail: on some Linux hosts the AppImage may need --no-sandbox; not
		// added here without evidence the bundled Electron requires it. If sandbox
		// launch failures appear, append "--no-sandbox" as the follow-up.
		err := c.deps.StartProcess(processStartConfig{
			Path: appPath,
			Args: []string{"--installed-via=npm-bootstrap"},
		})
		if err != nil {
			// Treat a launch failure as "not opened" so the caller prints the
			// manual-open path rather than aborting the whole command.
			return false, nil //nolint:nilerr // launch failure is reported via the bool, not as an error
		}
		return true, nil
	default:
		return false, nil
	}
}

// printDeprecationNotice explains the new role of the npm `ao` binary. Keep it
// honest: Track B (live auto-update) is not done, so it does not promise it.
func (c *commandContext) printDeprecationNotice(w io.Writer) {
	_, _ = fmt.Fprint(w, "Agent Orchestrator is now a desktop app, and the npm `ao` is just its launcher.\n"+
		"The app is distributed from the website and GitHub Releases; it owns the daemon and updates itself.\n"+
		"You can keep running `ao start` to fetch (if needed) and open it.\n")
}

// printManualOpen tells the user how to open the bundle when `ao start` could
// not launch it for them (non-darwin, or a failed launch handled upstream).
func (c *commandContext) printManualOpen(w io.Writer, appPath string) {
	_, _ = fmt.Fprintf(w, "Could not open the app automatically. Open it manually: %s\n", appPath)
}
