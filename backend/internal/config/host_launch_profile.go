package config

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// hostLaunchProfile is shared with Electron's host-launch-profile.ts. Never
// mutate process environment: CLI commands may resolve configuration repeatedly.
type hostLaunchProfile struct {
	DataDir string
	RunFile string
	Port    int
}

func loadHostLaunchProfile() (hostLaunchProfile, error) {
	return loadHostLaunchProfileForOS(runtime.GOOS)
}

func loadHostLaunchProfileForOS(platform string) (hostLaunchProfile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// Harness subprocesses may have no HOME while supplying both paths. There
		// is no profile location to read; retain this explicit-env-only workflow.
		if os.Getenv("AO_DATA_DIR") != "" && os.Getenv("AO_RUN_FILE") != "" {
			return hostLaunchProfile{Port: DefaultPort}, nil
		}
		return hostLaunchProfile{}, fmt.Errorf("resolve Icosa host profile: %w", err)
	}
	aoHome := filepath.Join(home, ".ao")
	defaults := hostLaunchProfile{DataDir: filepath.Join(aoHome, "dev", "data"), RunFile: filepath.Join(aoHome, "dev", "running.json"), Port: DefaultPort}
	// The local-distribution profile is supported on macOS/Linux only. Windows
	// has no POSIX mode/uid contract; ignore the file rather than pretend to
	// validate its ACL. Environment overrides are still applied by the caller.
	if platform == "windows" {
		return defaults, nil
	}
	file, err := os.Open(filepath.Join(aoHome, "icosa-desktop.json"))
	if os.IsNotExist(err) {
		return defaults, nil
	}
	if err != nil {
		return hostLaunchProfile{}, fmt.Errorf("read Icosa host profile: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return hostLaunchProfile{}, fmt.Errorf("stat Icosa host profile: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !hostProfileOwnedByCurrentUser(info) {
		return hostLaunchProfile{}, fmt.Errorf("icosa host launch profile must be owner-only (chmod 600)")
	}
	// Decode to the same JSON value model as the desktop parser; 1.0 is version 1.
	var p map[string]any
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&p); err != nil {
		return hostLaunchProfile{}, fmt.Errorf("invalid Icosa host profile: %w", err)
	}
	// json.Unmarshal/JSON.parse reject trailing content; match that here too.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return hostLaunchProfile{}, fmt.Errorf("invalid Icosa host profile: trailing JSON")
	}
	for key := range p {
		if key != "version" && key != "dataDir" && key != "runFile" && key != "port" {
			return hostLaunchProfile{}, fmt.Errorf("unknown Icosa profile field %q", key)
		}
	}
	profile := hostLaunchProfile{}
	for _, key := range []string{"dataDir", "runFile"} {
		value, ok := p[key].(string)
		if !ok || !filepath.IsAbs(value) || filepath.Clean(value) != value || !strings.HasPrefix(value, aoHome+string(filepath.Separator)) {
			return hostLaunchProfile{}, fmt.Errorf("icosa profile %s must be an absolute normalized path under ~/.ao", key)
		}
		if key == "dataDir" {
			profile.DataDir = value
		} else {
			profile.RunFile = value
		}
	}
	version, versionOK := p["version"].(float64)
	port, portOK := p["port"].(float64)
	if !versionOK || version != 1 || !portOK || math.Trunc(port) != port || port < 1024 || port > 65535 {
		return hostLaunchProfile{}, fmt.Errorf("invalid Icosa profile version or port")
	}
	profile.Port = int(port)
	return profile, nil
}
