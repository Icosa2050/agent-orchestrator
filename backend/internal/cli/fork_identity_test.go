package cli

import "testing"

func TestForkPackagedIdentity(t *testing.T) {
	if appBundleName != "Icosa Orchestrator.app" {
		t.Fatal(appBundleName)
	}
	if appStateFileName != "icosa-app-state.json" {
		t.Fatal(appStateFileName)
	}
	if linuxDesktopEntryName != "icosa-orchestrator.desktop" {
		t.Fatal(linuxDesktopEntryName)
	}
}
