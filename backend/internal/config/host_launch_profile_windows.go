package config

import "os"

// Windows profiles are ignored before any file is opened. Fail closed if a
// future caller tries to use POSIX ownership validation on Windows anyway.
func hostProfileOwnedByCurrentUser(_ os.FileInfo) bool { return false }
