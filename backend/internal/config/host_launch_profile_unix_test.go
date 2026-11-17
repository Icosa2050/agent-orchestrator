//go:build !windows

package config

import (
	"os"
	"syscall"
	"testing"
)

type profileFileInfo struct {
	os.FileInfo
	owner uint32
}

func (info profileFileInfo) Sys() any { return &syscall.Stat_t{Uid: info.owner} }
func TestForkHostLaunchProfileOwner(t *testing.T) {
	uid := os.Getuid()
	if !hostProfileOwnedByCurrentUser(profileFileInfo{owner: uint32(uid)}) {
		t.Fatal("current owner rejected")
	}
	if hostProfileOwnedByCurrentUser(profileFileInfo{owner: uint32(uid + 1)}) {
		t.Fatal("foreign owner accepted")
	}
}
