package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForkTriageTargetsFork(t *testing.T) {
	const fork = "Icosa2050/agent-orchestrator"
	if triageRepo != fork {
		t.Fatalf("triage repository = %q", triageRepo)
	}
	if prompt := triagePrompt(t.TempDir(), false); !strings.Contains(prompt, fork) || strings.Contains(prompt, "OrchestratorInc/agent-orchestrator") {
		t.Fatal("prompt lost fork target")
	}
	for _, repo := range []string{fork, "OrchestratorInc/agent-orchestrator"} {
		if got := triageIssueURL.MatchString("https://github.com/" + repo + "/issues/12"); got != (repo == fork) {
			t.Fatalf("returned URL for %s accepted=%v", repo, got)
		}
	}
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{
		".agents/skills/bug-triage/SKILL.md",
		"backend/internal/skillassets/bug-triage/SKILL.md",
		"backend/internal/skillassets/using-ao/commands/triage.md",
		"frontend/src/renderer/lib/report-problem.ts",
		"frontend/src/renderer/components/settings/ReportProblemContent.tsx",
	} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "bug-triage/SKILL.md") && !strings.Contains(string(raw), `[ "$ao_triage_repo" = "Icosa2050/agent-orchestrator" ]`) {
			t.Fatalf("%s must refuse a repository redirect", name)
		}
		if !strings.Contains(string(raw), fork) || strings.Contains(string(raw), "OrchestratorInc/agent-orchestrator") {
			t.Fatalf("%s must target fork only", name)
		}
	}
}
