package oci

import (
	"strings"
	"testing"
)

// The guarantee lives in these flags. If someone drops one, this fails.
func TestRunArgsCarryTheBoundary(t *testing.T) {
	n := &Net{Sweep: "s1", Subnet: "10.111.0.0/24", GW: "10.111.0.1", Port: 8080}
	got := strings.Join(n.runArgs("abc123", "img", []string{"/work/task:/work"}, []string{"agent"}), " ")
	for _, want := range []string{
		"--network glue-s1",
		"--cap-drop NET_ADMIN",
		"--cap-drop NET_RAW",
		"--security-opt no-new-privileges",
		"--label glue.sweep=s1",
		"ANTHROPIC_BASE_URL=http://10.111.0.1:8080/s/abc123",
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
