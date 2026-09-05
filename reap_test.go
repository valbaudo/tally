package dawn

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// parseReapable is the whole reap predicate, and it is asserted against the
// real shape of `docker ps -a --filter label=com.docker.compose.project
// --format '{{.Label "com.docker.compose.project"}}|{{.State}}'` output,
// including the fixture this ticket names by name: a real hand-run orphan
// this tool did not create (mdash__hagmszn__verifier__trial) must survive
// because it does not carry the dawn__ prefix at all. dawn's own long-lived
// Postgres container carries no compose label whatsoever, so it never
// appears in this input in the first place — nothing to assert there.
//
// dawn__abc1234__env and dawn__abc1234__verifier__k1 are two DIFFERENT
// compose projects belonging to the same trial (its agent environment and
// its separate verifier), each with a single container — reap decides them
// independently, project by project, exactly as docker compose down does.
func TestParseReapableMatchesOnlyExitedDawnPrefixedProjects(t *testing.T) {
	const psOutput = `mdash__hagmszn__verifier__trial|exited
dawn__abc1234__env|exited
dawn__abc1234__verifier__k1|exited
dawn__running1__env|running
dawn__dead1__env|dead
`
	got := parseReapable(psOutput)
	want := []string{"dawn__abc1234__env", "dawn__abc1234__verifier__k1", "dawn__dead1__env"}
	if !equalStrings(got, want) {
		t.Fatalf("parseReapable() = %v, want %v", got, want)
	}
	for _, mustSurvive := range []string{"mdash__hagmszn__verifier__trial", "dawn__running1__env"} {
		for _, p := range got {
			if p == mustSurvive {
				t.Fatalf("parseReapable() swept %q, which must survive", mustSurvive)
			}
		}
	}
}

// A single project with more than one container — e.g. a main container plus
// Harbor's egress-control sidecar, both carrying the SAME project label — is
// left alone in its ENTIRETY the instant any one of them is alive: docker
// compose down acts on the whole project, so "reapable" has to mean "every
// container THIS project has is done", not "at least one is".
func TestParseReapableExcludesTheWholeProjectIfAnyOfItsContainersIsAlive(t *testing.T) {
	got := parseReapable("dawn__mixed__env|exited\ndawn__mixed__env|running\n")
	if len(got) != 0 {
		t.Fatalf("parseReapable() = %v, want none: one live container must veto the whole project", got)
	}
}

// A malformed or unlabelled line (no "|", or an empty label before it) must
// not panic and must not be mistaken for a dawn__ project.
func TestParseReapableIgnoresUnlabelledLines(t *testing.T) {
	got := parseReapable("no-pipe-in-this-line\n|exited\ndawn__x__env|exited\n")
	if len(got) != 1 || got[0] != "dawn__x__env" {
		t.Fatalf("parseReapable() = %v, want just [dawn__x__env]", got)
	}
}

func TestParseReapableEmptyInput(t *testing.T) {
	if got := parseReapable(""); len(got) != 0 {
		t.Fatalf("parseReapable(\"\") = %v, want none", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- live docker tests: real compose projects, real containers ---

func hasDocker() bool {
	return exec.Command("docker", "info").Run() == nil
}

// composeUp starts a minimal one-container compose project under the given
// project name using the real docker CLI, and registers a best-effort
// cleanup. sleepSeconds 0 makes the container exit almost immediately;
// otherwise it stays Running for the life of the test.
func composeUp(t *testing.T, project string, sleepSeconds int) {
	t.Helper()
	dir := t.TempDir()
	compose := fmt.Sprintf("services:\n  main:\n    image: alpine:3.20\n    command: [\"sleep\", \"%d\"]\n", sleepSeconds)
	if err := os.WriteFile(dir+"/docker-compose.yml", []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "-p", project, "up", "-d")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose -p %s up: %v: %s", project, err, out)
	}
	t.Cleanup(func() {
		// Best-effort: the test may already have torn this down via reap()
		// itself, in which case this is the measured no-op.
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
	})
}

func waitExited(t *testing.T, project string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
			"--format", "{{.State}}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "exited" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("project %s never reached exited", project)
}

func projectExists(t *testing.T, project string) bool {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
		"--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return strings.TrimSpace(string(out)) != ""
}

// TestReapSweepsExitedDawnProjectAndLeavesForeignOrphanAlone is the real,
// docker-backed proof of Decision 1's critical requirement: a project this
// tool did not create must survive. It builds its own synthetic "foreign"
// orphan (rather than depending on any specific container that happens to
// exist on the machine running the test) so the test is reproducible
// anywhere docker runs, alongside a real dawn__-prefixed exited project to
// prove the positive case in the same run.
func TestReapSweepsExitedDawnProjectAndLeavesForeignOrphanAlone(t *testing.T) {
	if !hasDocker() {
		t.Skip("docker not available")
	}
	dawnProject := "dawn__reaptest1234__env"
	foreignProject := "not-dawn-reaptest-foreign"

	composeUp(t, dawnProject, 0)
	composeUp(t, foreignProject, 0)
	waitExited(t, dawnProject)
	waitExited(t, foreignProject)

	if err := reap(); err != nil {
		t.Fatalf("reap(): %v", err)
	}

	if projectExists(t, dawnProject) {
		t.Errorf("reap() left %q behind", dawnProject)
	}
	if !projectExists(t, foreignProject) {
		t.Errorf("reap() swept %q, a project it did not create", foreignProject)
	}
}

// TestReapNeverTouchesARunningProject is the negative case that makes the
// whole design safe: a live sibling dawn run's containers are Running, and
// reap must leave them exactly alone, mid-run, with no coordination at all.
func TestReapNeverTouchesARunningProject(t *testing.T) {
	if !hasDocker() {
		t.Skip("docker not available")
	}
	project := "dawn__reaptest-running__env"
	composeUp(t, project, 60)

	if err := reap(); err != nil {
		t.Fatalf("reap(): %v", err)
	}
	if !projectExists(t, project) {
		t.Fatalf("reap() swept a RUNNING project: never")
	}
}
