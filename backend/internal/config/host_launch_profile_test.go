package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestForkHostLaunchProfileVectors(t *testing.T) {
	data, err := os.ReadFile("../../../test/fixtures/icosa-host-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name    string
		Absent  bool
		Valid   bool
		Profile any
		Raw     string
		Env     map[string]string
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			expand := func(s string) string {
				return strings.ReplaceAll(strings.ReplaceAll(s, "/", string(filepath.Separator)), "__HOME__", home)
			}
			for _, key := range []string{"AO_DATA_DIR", "AO_RUN_FILE", "AO_PORT"} {
				t.Setenv(key, expand(v.Env[key]))
			}
			profile, _ := v.Profile.(map[string]any)
			for key, value := range profile {
				if s, ok := value.(string); ok {
					profile[key] = expand(s)
				}
			}
			if !v.Absent {
				dir := filepath.Join(home, ".ao")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(v.Profile)
				if err != nil {
					t.Fatal(err)
				}
				if v.Raw != "" {
					raw = []byte(v.Raw)
				}
				if err := os.WriteFile(filepath.Join(dir, "icosa-desktop.json"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			state, dataDir, runFile, pathErr := ResolvePaths()
			if !v.Valid {
				if err == nil || pathErr == nil {
					t.Fatalf("invalid profile accepted: Load=%v ResolvePaths=%v", err, pathErr)
				}
				return
			}
			if err != nil || pathErr != nil {
				t.Fatalf("Load=%v ResolvePaths=%v", err, pathErr)
			}
			wantData, wantRun, wantPort := filepath.Join(home, ".ao", "dev", "data"), filepath.Join(home, ".ao", "dev", "running.json"), 3002
			if !v.Absent {
				wantData = profile["dataDir"].(string)
				wantRun = profile["runFile"].(string)
				wantPort = int(profile["port"].(float64))
			}
			if s := os.Getenv("AO_DATA_DIR"); s != "" {
				wantData = s
			}
			if s := os.Getenv("AO_RUN_FILE"); s != "" {
				wantRun = s
			}
			if os.Getenv("AO_PORT") != "" {
				wantPort = 4500
			}
			if cfg.DataDir != wantData || cfg.RunFilePath != wantRun || cfg.Port != wantPort || dataDir != wantData || runFile != wantRun || state != wantData {
				t.Fatalf("cfg paths=%q %q port=%d ResolvePaths=%q %q %q; want %q %q %d", cfg.DataDir, cfg.RunFilePath, cfg.Port, state, dataDir, runFile, wantData, wantRun, wantPort)
			}
			for key, value := range v.Env {
				if os.Getenv(key) != expand(value) {
					t.Fatalf("resolver mutated %s", key)
				}
			}
		})
	}
}

func TestForkHostLaunchProfilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"AO_DATA_DIR", "AO_RUN_FILE", "AO_PORT"} {
		t.Setenv(key, "")
	}
	dir := filepath.Join(home, ".ao")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "dataDir": filepath.Join(dir, "data"), "runFile": filepath.Join(dir, "run.json"), "port": 4002})
	file := filepath.Join(dir, "icosa-desktop.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o644, 0o660, 0o604} {
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted unsafe permissions %o", mode)
		}
		if _, _, _, err := ResolvePaths(); err == nil {
			t.Fatalf("paths accepted unsafe permissions %o", mode)
		}
	}
}

func TestForkHostLaunchProfileWithoutHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix HOME lookup")
	}
	t.Setenv("HOME", "")
	t.Setenv("AO_PORT", "")
	t.Setenv("AO_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("AO_RUN_FILE", filepath.Join(t.TempDir(), "running.json"))
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ResolvePaths(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_RUN_FILE", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing HOME and run-file override accepted")
	}
}
