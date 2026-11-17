package config

import "os"

// Match Electron on Windows, where process.getuid is unavailable.
func hostProfileOwnedByCurrentUser(_ os.FileInfo) bool { return true }
