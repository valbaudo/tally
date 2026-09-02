package oci

import (
	"strings"
	"testing"
)

// The guarantee lives in these flags. If someone drops one, this fails.
func TestRunArgsCarryTheBoundary(t *testing.T) {
	n := &Net{Sweep: "s1", Subnet: "10.111.0.0/24", GW: "10.111.0.1", Port: 8080}
	env := []string{"ANTHROPIC_BASE_URL=http://10.111.0.1:8080/s/abc123/anthropic"}
	got := strings.Join(n.runArgs("abc123", "img", env, []string{"/work/task:/work"}, []string{"agent"}), " ")
	for _, want := range []string{
		"--network glue-s1",
		"--cap-drop NET_ADMIN",
		"--cap-drop NET_RAW",
		"--security-opt no-new-privileges",
		"--label glue.sweep=s1",
		"ANTHROPIC_BASE_URL=http://10.111.0.1:8080/s/abc123/anthropic",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in: %s", want, got)
		}
	}
	if strings.Contains(got, "--network host") || strings.Contains(got, "-p ") {
		t.Fatalf("container must not be reachable from outside the sweep: %s", got)
	}
	// The container never gets the database, at any permission.
	for _, forbidden := range []string{"glue.db", "docker.sock"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("%q reached the container argv: %s", forbidden, got)
		}
	}
}

// A validator container executes attacker-controlled code. It must have no
// network at all, and it must still carry the label Reap collects by —
// otherwise it outlives the sweep that started it.
func TestSandboxArgsHaveNoNetworkAndAreStillReapable(t *testing.T) {
	n := &Net{Sweep: "s1", Subnet: "10.111.0.0/24", GW: "10.111.0.1", Port: 8080}
	got := strings.Join(n.sandboxArgs("abc123", "img", []string{"/ws:/work:ro"}, []string{"/poc"}), " ")
	for _, want := range []string{
		"--network none",
		"--label glue.sweep=s1",
		"--cap-drop NET_ADMIN",
		"--cap-drop NET_RAW",
		"--security-opt no-new-privileges",
		"--rm",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in: %s", want, got)
		}
	}
	// No route to the proxy means no credential to reach, and nothing to
	// spend: a sandbox container is unmetered because it is unnetworked.
	if strings.Contains(got, "BASE_URL") || strings.Contains(got, "glue-s1") {
		t.Fatalf("sandbox reached the sweep network: %s", got)
	}
}
