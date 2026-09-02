package glue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/valbaudo/dawn/internal/oci"
	"github.com/valbaudo/dawn/internal/proxy"
	"github.com/valbaudo/dawn/internal/store"
	"github.com/valbaudo/dawn/internal/tui"
)

// Config is everything a sweep needs. Everything with a sane default has one.
type Config struct {
	// Sweep is a stable id, and stability is the point: it names the reap
	// scope, so a supervisor that restarts after a crash reaps the containers
	// and closes the spans that its own previous incarnation left behind. A
	// per-process id would have nothing of its own to reap. Two supervisors on
	// one sweep id is refused by an flock, not by a lease.
	Sweep string

	// DB is the path to the ledger file. It is created if absent, and it is
	// never mounted into a container.
	DB string

	// APIKey is the real Anthropic credential. It stays in this process's
	// memory and is attached to outbound requests by the proxy; no container
	// ever sees it.
	APIKey string

	// Image is the agent container image.
	//
	// Leaving it empty declares a sweep that launches no containers, and no
	// container network is then created: there is nothing to isolate. Such a
	// sweep still meters, prices and ledgers every call, because a predicate a
	// harness runs in its own process goes through the same proxy an agent
	// does. It is also the only shape that works on macOS, where the bridge
	// gateway lives inside a VM the host cannot bind. Launch returns an error.
	Image string

	// Upstream overrides the API endpoint the proxy forwards to. Empty means
	// https://api.anthropic.com. It exists because "which endpoint" is a
	// deployment fact — a regional gateway, a replay server, a staging
	// account — and a deployment fact belongs in config, not in an env var the
	// process reads behind its own back.
	Upstream string

	// Budget is the sweep-wide cap. Salvage is the part of it held back until
	// Budget().Claim(); the effective cap until then is Budget - Salvage.
	Budget, Salvage USD

	// Subnet, GW and Port describe the sweep's private network. GW is the
	// address the supervisor binds, and it is inside Subnet — which is exactly
	// why a container with no default route can still reach it. The gateway is
	// on-link, one ARP away; everything else has no route at all.
	Subnet, GW string
	Port       int

	// Verify runs the network boundary predicate in a throwaway container
	// before any agent starts, and refuses to continue if the boundary the
	// design claims is not the boundary that exists. This matters more than it
	// looks: on Docker Engine < 26 the very same flags produce a network with a
	// default route, DROP-based egress and working external DNS. Same commands,
	// boundary gone, silently. A version string is not evidence. Two seconds.
	Verify bool
}

func (c *Config) defaults() {
	if c.Image == "" {
		// No containers, so no bridge, so nothing to bind but loopback.
		if c.GW == "" {
			c.GW = "127.0.0.1"
		}
		return
	}
	if c.Subnet == "" {
		c.Subnet = "10.111.0.0/24"
	}
	if c.GW == "" {
		c.GW = "10.111.0.1"
	}
	if c.Port == 0 {
		c.Port = 8080
	}
}

// Sweep is the supervisor: one ordinary Go process that owns the ledger, the
// listener and every container.
//
// There is no lease, no TTL and no heartbeat anywhere in this design, because
// three real leases already exist and cost nothing:
//
//   - The flock on the sweep's lock file is the ledger's lease. The kernel
//     drops it when the process dies.
//   - The container is the agent's lease. It carries the sweep label, and two
//     commands at boot collect every container the last supervisor left.
//   - The open HTTP connection is a call's lease. When the agent goes away, the
//     request context is cancelled and the row is written on the way out with
//     usage_complete=0 and the reservation as the charge.
//
// A TTL would be a fourth mechanism saying approximately what those three say
// exactly, and it would introduce the one failure mode they cannot have: a live
// worker reaped for being slow. Crash cleanup is two statements at boot, not a
// reconciler that has to keep running to stay correct.
//
// One thing worth stating precisely, because it is easy to over-claim: the
// supervisor's death does not kill the containers. They are children of
// dockerd, not of us. It SILENCES them — their only route out was the listener
// that just died, so an orphaned agent burns CPU and cannot spend a cent, and
// the next boot's reap collects it.
type Sweep struct {
	*Ledger

	cfg Config
	net *oci.Net
	pr  *proxy.Proxy
	ln  net.Listener
	srv *http.Server
}

// Open brings the sweep up, in the order the guarantees require:
//
//  1. take the ledger's flock, so no second supervisor can reap our spans;
//  2. reap — close every span the last incarnation left open, and destroy every
//     container it left running;
//  3. create the private network and, if asked, prove the boundary;
//  4. bind the listener.
//
// The listener is bound here rather than in Serve so that Open returning
// successfully means an agent launched on the very next line has somewhere to
// talk to. Address-in-use surfaces as an error from Open, not as a race with a
// container that has already started.
func Open(cfg Config) (*Sweep, error) {
	cfg.defaults()
	if cfg.Sweep == "" || cfg.DB == "" || cfg.APIKey == "" {
		return nil, errors.New("glue: Config needs Sweep, DB and APIKey")
	}

	db, err := store.Open(cfg.DB, cfg.Sweep)
	if err != nil {
		return nil, err
	}
	// Statement one of the two-statement crash cleanup.
	if _, err := db.Reap(); err != nil {
		db.Close()
		return nil, err
	}

	l := &Ledger{
		db:    db,
		spans: map[string]*Span{},
		caps:  map[string]USD{},
	}
	if err := db.NewPool(cfg.Sweep, "", cfg.Budget, cfg.Salvage); err != nil {
		db.Close()
		return nil, err
	}
	l.root = &Pool{l: l, name: cfg.Sweep, cap: cfg.Budget}

	s := &Sweep{
		Ledger: l,
		cfg:    cfg,
		net:    &oci.Net{Sweep: cfg.Sweep, Subnet: cfg.Subnet, GW: cfg.GW, Port: cfg.Port},
	}
	if cfg.Image != "" {
		// Statement two, plus the network this sweep's containers live on.
		if err := s.net.Up(); err != nil {
			db.Close()
			return nil, err
		}
		if cfg.Verify {
			if err := s.net.Verify(); err != nil {
				s.net.Reap()
				db.Close()
				return nil, err
			}
		}
	}

	// The proxy resolves span ids through l.target and writes through store. It
	// cannot name Span, Pool or Ledger — the import runs the other way — which
	// is the same reason a harness cannot reach the metering path.
	s.pr = proxy.New(db, cfg.APIKey, l.target)
	if cfg.Upstream != "" {
		u, err := url.Parse(cfg.Upstream)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("glue: Upstream: %w", err)
		}
		s.pr.Upstream = u
	}
	mux := http.NewServeMux()
	mux.Handle("/s/", s.pr)
	s.srv = &http.Server{Handler: mux}

	// Bind the gateway, not 0.0.0.0: the proxy must be reachable from this
	// sweep's containers and from this host, and from nowhere else.
	s.ln, err = net.Listen("tcp", s.net.Addr())
	if err != nil {
		s.net.Reap()
		db.Close()
		return nil, fmt.Errorf("glue: cannot bind %s: %w", s.net.Addr(), err)
	}
	// The base URL every container is handed is read back off the listener, not
	// recomputed from Config, so it cannot disagree with what is actually bound
	// (Port 0 being the case where it obviously would).
	l.base = "http://" + s.ln.Addr().String()
	return s, nil
}

// Serve runs the listener until ctx is done. It blocks; run it in a goroutine.
//
// The http.Server carries no ReadTimeout, WriteTimeout or IdleTimeout, and that
// is deliberate. A single Messages request with extended thinking legitimately
// runs for many minutes, and a WriteTimeout would sever it mid-stream — which
// the ledger would then faithfully record as class=Cancelled,
// outcome="stream_incomplete", charged at the full reservation. We would be
// paying for, and recording, a failure we caused. The lease on a call is the
// connection, and the agent is the one holding it.
func (s *Sweep) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(sh)
	}()
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Addr is the address the listener is bound to, which is also the address every
// container's ANTHROPIC_BASE_URL points at.
func (s *Sweep) Addr() string { return s.ln.Addr().String() }

// Path is the ledger file. It is a path, not a handle: another process can open
// it read-only — which is exactly what `glue top` and `glue table` do, and why
// WAL is on — but nothing inside this one can be handed a writable *sql.DB.
func (s *Sweep) Path() string { return s.cfg.DB }

// Budget is the sweep's root pool. Every Sub descends from it, and every charge
// anywhere in the tree is charged against it in the same statement.
func (s *Sweep) Budget() *Pool { return s.root }

// Launch starts one agent container for one span and returns the container id.
//
// mounts are host:container[:ro] volume specs — a list of volume specs, not a
// list of docker flags, because a caller that could pass arbitrary argv could
// pass --network host and delete the boundary this package exists to enforce.
// There is no argument here that can reach the ledger file or the docker
// socket.
//
// The container gets the span's Env and nothing else. It has no default route,
// so a hardcoded api.anthropic.com fails with ENETUNREACH before a packet is
// built — at the routing layer, instantly, rather than as a firewall timeout
// that would change how the agent behaves by hanging it. It cannot resolve any
// name outside the sweep. It cannot reach its siblings. NET_ADMIN and NET_RAW
// are dropped, so root inside it cannot add the route back.
func (s *Sweep) Launch(span *Span, mounts []string, cmd ...string) (string, error) {
	if s.cfg.Image == "" {
		return "", errors.New("glue: Config.Image is empty; this sweep launches no containers")
	}
	id, err := s.net.Launch(span.id, s.cfg.Image, mounts, cmd...)
	if err != nil {
		return "", err
	}
	span.Fact("container", []byte(id))
	return id, nil
}

// Kill stops one agent's container. It does not close the span: the harness
// decides the class, because the harness is the only thing that knows whether a
// kill was a deadline, a gate, or a crash.
func (s *Sweep) Kill(span *Span) error { return s.net.Kill(span.id) }

// InFlight reports how many upstream requests are open on a span right now.
//
// This is the liveness signal that costs nothing and cannot be wrong: the
// supervisor is holding the request, so a span with a call in flight is alive
// by construction, for exactly as long as the call runs. It is what keeps a
// twelve-minute call from reading as stale, with no threshold tuned to model
// latency and no heartbeat process inside any agent image.
func (s *Sweep) InFlight(span *Span) int { return s.pr.InFlight(span.id) }

// Close shuts the listener, destroys this sweep's containers and network, and
// releases the ledger.
//
// Spans still open at this point are left open on disk. The next boot's reap
// closes them as class=Unknown, outcome="abandoned", which is a truer record
// than closing them here with a verdict nobody actually reached.
func (s *Sweep) Close() error {
	s.srv.Close()
	var err error
	if s.cfg.Image != "" {
		err = s.net.Reap()
	}
	if dberr := s.db.Close(); err == nil {
		err = dberr
	}
	return err
}

// Report renders the sweep's submission table: the human lines are returned,
// and the machine-readable CSV is written to csv if it is non-nil. The numbers
// are the same numbers, so the file a leaderboard reads is never a re-typing of
// what a human read on screen.
//
// This is a method rather than a function taking a *sql.DB because a harness
// does not get a *sql.DB — see the package doc. The alternative was exporting
// the handle "just for reporting", which is exactly the crack every
// unfalsifiable promise gets through.
func (s *Sweep) Report(csv io.Writer) ([]string, error) {
	s.mu.RLock()
	caps := make(map[string]USD, len(s.caps))
	for k, v := range s.caps {
		caps[k] = v
	}
	s.mu.RUnlock()

	nodes, err := tui.Load(s.db.SQL(), s.cfg.Sweep, caps)
	if err != nil {
		return nil, err
	}
	rows, err := tui.Table(s.db.SQL(), s.cfg.Sweep, nodes)
	if err != nil {
		return nil, err
	}
	if csv != nil {
		if err := tui.TableCSV(csv, rows); err != nil {
			return nil, err
		}
	}
	return tui.TableLines(s.cfg.Sweep, rows), nil
}

// The keys glue table reads out of Fact rows. They are re-exported here because
// they are part of the contract between a harness and the report, and a harness
// cannot import the package that renders it.
const (
	FactTask    = tui.FactTask    // "arvo:10400"
	FactLevel   = tui.FactLevel   // CyberGym difficulty level
	FactPoCHash = tui.FactPoCHash // sha256 of the submitted crash input
	FactPoCLen  = tui.FactPoCLen  // its length in bytes
	FactVulExit = tui.FactVulExit // exit code against the vulnerable build
	FactFixExit = tui.FactFixExit // exit code against the patched build
)
