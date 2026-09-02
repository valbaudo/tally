package oci

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Net is the sweep's network boundary.
//
// One Docker bridge network created with --internal. Docker gives such a
// network a gateway address on the host but *no default route inside the
// container* (moby#47356, Engine >= 26.0), so a container on it can reach
// exactly one address — the gateway — and every other destination fails at
// the routing layer with ENETUNREACH before a packet is built. The supervisor
// binds that gateway address. That is the whole mechanism: no iptables of our
// own, no veth surgery, no nftables ruleset to get wrong.
//
// ponytail: Docker's own --internal is rung 4 of the ladder. We do not
// re-implement it with veth pairs; we assert it with Verify at boot.
type Net struct {
	Sweep  string // sweep id; names the network and labels every container
	Subnet string // e.g. "10.111.0.0/24"
	GW     string // e.g. "10.111.0.1" — the address the supervisor binds
	Port   int    // supervisor listener port
}

func (n *Net) name() string { return "glue-" + n.Sweep }

// Addr is what the supervisor passes to net.Listen. Binding the gateway
// rather than 0.0.0.0 keeps the proxy off the host's real NICs.
func (n *Net) Addr() string { return fmt.Sprintf("%s:%d", n.GW, n.Port) }

// BaseURL is the value of ANTHROPIC_BASE_URL for one span. SDKs append
// /v1/messages, so the span id survives with zero client cooperation.
func (n *Net) BaseURL(span string) string {
	return fmt.Sprintf("http://%s:%d/s/%s", n.GW, n.Port, span)
}

// Up reaps anything left by a previous supervisor with this sweep id and
// creates the network. Both statements are the crash cleanup.
func (n *Net) Up() error {
	if err := n.Reap(); err != nil {
		return err
	}
	_, err := docker("network", "create",
		"--driver", "bridge",
		"--internal",
		"--subnet", n.Subnet,
		"--gateway", n.GW,
		"--opt", "com.docker.network.bridge.name=glue-"+n.Sweep,
		"--opt", "com.docker.network.bridge.enable_icc=false",
		n.name())
	return err
}

// Reap is boot cleanup: kill every container this sweep id ever labelled,
// then drop the network. Idempotent; a missing network is not an error.
func (n *Net) Reap() error {
	out, err := docker("ps", "-aq", "--filter", "label=glue.sweep="+n.Sweep)
	if err != nil {
		return err
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		if _, err := docker(append([]string{"rm", "-f"}, ids...)...); err != nil {
			return err
		}
	}
	if _, err := docker("network", "rm", n.name()); err != nil &&
		!strings.Contains(err.Error(), "not found") {
		return err
	}
	return nil
}

// runArgs builds the argv for one agent container. Kept pure so the flags
// that carry the guarantee are testable without a daemon.
//
// mounts is the only per-container knob, and it is deliberately a list of
// volume specs rather than a list of docker flags: a caller that could pass
// arbitrary argv could pass --network host and delete the boundary this file
// exists to enforce.
func (n *Net) runArgs(span, image string, mounts, cmd []string) []string {
	a := []string{"run", "-d",
		"--name", "glue-" + span,
		"--label", "glue.sweep=" + n.Sweep,
		"--label", "glue.span=" + span,
		"--network", n.name(),
		// The route table is the boundary; NET_ADMIN would let root in the
		// container edit it, NET_RAW would let it forge frames on the bridge.
		"--cap-drop", "NET_ADMIN",
		"--cap-drop", "NET_RAW",
		"--security-opt", "no-new-privileges",
		"-e", "ANTHROPIC_BASE_URL=" + n.BaseURL(span),
	}
	for _, m := range mounts {
		a = append(a, "-v", m)
	}
	a = append(a, image)
	return append(a, cmd...)
}

// Launch starts one agent container. mounts are host:container[:ro] specs.
func (n *Net) Launch(span, image string, mounts []string, cmd ...string) (string, error) {
	out, err := docker(n.runArgs(span, image, mounts, cmd)...)
	return strings.TrimSpace(out), err
}

// Kill stops and removes one agent's container. `docker rm -f` rather than
// `docker kill` so a container that is already dead but not yet collected takes
// the same path as a live one; there is no "was it running?" branch.
func (n *Net) Kill(span string) error {
	_, err := docker("rm", "-f", "glue-"+span)
	return err
}

// Predicate runs inside a live container and exits non-zero the moment the
// boundary the design claims is not the boundary that exists.
const Predicate = `set -eu
# 1. exactly one IPv4 route, and it is the sweep subnet, on-link.
[ "$(ip -4 route show | wc -l)" -eq 1 ]
ip -4 route show | grep -q "^$SUB dev eth0 .*scope link"
# 2. no default route, either family.
[ -z "$(ip -4 route show default)" ]
[ -z "$(ip -6 route show default 2>/dev/null || true)" ]
# 3. a hardcoded IP fails at the ROUTE layer, immediately. A firewall DROP
#    would instead burn the full -T 2 timeout, and this line would fail.
wget -T 2 -O /dev/null http://1.1.1.1/ 2>&1 | grep -qi unreachable
# 4. the supervisor is reachable. Any HTTP status proves the TCP path.
wget -T 5 -S -O /dev/null "http://$GW:$PORT/" 2>&1 | grep -q "HTTP/1"
# 5. no name outside the sweep resolves.
! nslookup api.anthropic.com >/dev/null 2>&1
# 6. no path resolves to the ledger or to the daemon.
! grep -Eq "glue\\.db|db-wal|db-shm|docker\\.sock" /proc/self/mountinfo
echo BOUNDARY-OK`

// Verify runs Predicate in a throwaway container on the sweep network. The
// supervisor calls this once at boot and refuses to launch agents if it
// fails. Engine version strings are not evidence; this is.
func (n *Net) Verify() error {
	out, err := docker("run", "--rm",
		"--label", "glue.sweep="+n.Sweep,
		"--network", n.name(),
		"--cap-drop", "NET_ADMIN", "--cap-drop", "NET_RAW",
		"-e", "GW="+n.GW, "-e", "SUB="+n.Subnet,
		"-e", fmt.Sprintf("PORT=%d", n.Port),
		"alpine:3", "sh", "-c", Predicate)
	if err != nil {
		return fmt.Errorf("network boundary predicate failed: %w", err)
	}
	if !strings.Contains(out, "BOUNDARY-OK") {
		return fmt.Errorf("network boundary predicate produced %q", out)
	}
	return nil
}

func docker(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := exec.Command("docker", args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Wait blocks until the container exits and returns its exit code.
//
// On context cancellation it kills the container and returns ctx.Err(). That
// ordering is the point: the container is the lease, so cancelling the caller
// has to actually stop the spend. Returning early and leaving the container
// running is how a cancelled sweep keeps billing.
func (n *Net) Wait(ctx context.Context, id string) (int, error) {
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, err := docker("wait", id)
		if err != nil {
			done <- result{0, err}
			return
		}
		code, err := strconv.Atoi(strings.TrimSpace(out))
		done <- result{code, err}
	}()
	select {
	case r := <-done:
		return r.code, r.err
	case <-ctx.Done():
		_, _ = docker("kill", id)
		<-done // do not leak the goroutine; docker wait returns once killed
		return 0, ctx.Err()
	}
}
