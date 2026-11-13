package telemetry

import (
	"os"
	"path/filepath"
	"runtime"
)

// OptOutMarkerPresent fails closed, including dangling symlinks and I/O errors.
func OptOutMarkerPresent(dataDir string) bool {
	_, err := os.Lstat(filepath.Join(dataDir, OptOutFile))
	return !os.IsNotExist(err)
}

// AnalyticsConsentGranted never infers consent from a packaged build or env.
func AnalyticsConsentGranted(dataDir string) bool {
	if OptOutMarkerPresent(dataDir) {
		return false
	}
	file := filepath.Join(dataDir, OptInFile)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return false
	}
	raw, err := os.ReadFile(file)
	return err == nil && string(raw) == "1\n"
}
