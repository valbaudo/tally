package dawn

// reap.go: the restart sweep. Every Harbor trial dawn dispatches generates
// its task directory named "dawn" (runTrial, harbor.go); Harbor's
// LocalTaskId.get_name() takes that basename verbatim as the seed for
// trial_name ("{name[:32]}__{shortuuid7}"), and every compose project a
// trial's environments open suffix off that name ("…__env",
// "…__verifier__<key>"). So every container a dawn TRIAL creates carries a
// com.docker.compose.project label starting with "dawn__" — one string, no
// new state, riding a naming mechanism Harbor already has rather than
// inventing a label of dawn's own.
//
// A trial's containers, and only those. dawn makes two docker calls of its
// own, outside Harbor and so outside compose: proveGate's gate selftest
// (docker run --rm --network=none) and deriveGate's build. Neither carries a
// compose project, so neither is in the reap set, and this said "every
// container dawn ever creates" as though they were. Both are --rm or
// build-scoped, so only a hard kill of dawn's own process mid-call leaks
// one, and proveGate memoises per image, which bounds even that. Labelling
// them would mean fabricating a compose project for a container that has no
// compose file, to sweep a leak narrower than the races documented below.
//
// This is a STRING MATCH, not a cryptographic tag: a Harbor task directory
// named "dawn" by anything other than this package would pollute the reap
// set. Nothing here can tell the difference.

import (
	"fmt"
	"os/exec"
	"strings"
)

// reapPrefix is the literal compose-project prefix every dawn-created
// container carries: generatedTaskDirName (harbor.go) is the task directory
// basename Harbor's LocalTaskId.get_name() reads, "__" is Harbor's own
// trial_name separator (generate_trial_name in harbor's TrialConfig). Tied to
// the constant, not re-typed, so the two cannot silently drift apart.
const reapPrefix = generatedTaskDirName + "__"

// reap tears down every dawn__ compose project whose containers are ALL
// Exited or Dead, and their networks (docker compose down's default; nothing
// here declares a volume, so none is removed). It never touches a project
// with so much as one Running container — that predicate is the whole safety
// argument: a live sibling dawn run's containers are Running, so two dawn
// processes in flight at once are safe by construction, with no lock and no
// "am I alone" check. Sweeping unconditionally on every Main() is then
// idempotent: nothing to reap is a no-op.
//
// Accepted costs, paid on every invocation: a docker-ps-class call plus a
// Docker-availability dependency at startup, and two dawn processes
// crash-restarting at the same moment can race to tear down the same exited
// project — harmless, since `docker compose down` on an absent project is a
// no-op (measured against a real project with no live compose file backing
// it — labels alone are enough), but it is real Docker traffic, not a
// no-op call.
func reap() error {
	projects, err := reapableProjects()
	if err != nil {
		return err
	}
	for _, p := range projects {
		// --remove-orphans catches a sidecar container sharing the project
		// label without being one of the compose file's own services (Harbor
		// adds one for egress control). down's own default already removes
		// the project's networks.
		cmd := exec.Command("docker", "compose", "-p", p, "down", "--remove-orphans")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("dawn: reap: docker compose -p %s down: %w: %s", p, err, out)
		}
	}
	return nil
}

// reapableProjects asks docker which dawn__ projects exist and are all
// Exited/Dead, and returns them. The docker-shelling half is inherently
// untestable without a daemon; parseReapable below is the whole predicate,
// pulled out so it can be tested on plain text.
func reapableProjects() ([]string, error) {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}|{{.State}}`,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("dawn: reap: docker ps: %w", err)
	}
	return parseReapable(string(out)), nil
}

// parseReapable is reapableProjects' predicate: every dawn__-prefixed
// project name, in first-seen order, for which NO container reported a state
// other than "exited" or "dead" — one Running (or Created, Paused,
// Restarting...) container anywhere in the project takes the whole project
// off the list, never just that one container, because `docker compose down`
// itself acts on the whole project.
func parseReapable(psOutput string) []string {
	var order []string
	live := map[string]bool{} // project -> has a container that is not exited/dead
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(psOutput), "\n") {
		project, state, ok := strings.Cut(line, "|")
		if !ok || !strings.HasPrefix(project, reapPrefix) {
			continue
		}
		if !seen[project] {
			seen[project] = true
			order = append(order, project)
		}
		switch strings.ToLower(state) {
		case "exited", "dead":
		default:
			live[project] = true
		}
	}
	var reapable []string
	for _, p := range order {
		if !live[p] {
			reapable = append(reapable, p)
		}
	}
	return reapable
}
