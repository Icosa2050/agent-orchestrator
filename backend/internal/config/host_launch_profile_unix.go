//go:build !windows

package config

import (
	"os"
	"syscall"
)

func hostProfileOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Getuid())
}
