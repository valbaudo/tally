package tui

// Open span != live work.
//
// An open span proves a harness called Ledger.Span(). It does not prove the
// container is doing anything. The staleness rule has to separate three things
// a single timeout cannot:
//
//	an agent waiting twelve minutes on one Opus call        -> alive, silent
//	an agent compiling a fuzz target between calls          -> alive, silent
//	an agent wedged on a dead socket                        -> not alive, silent
//
// All three write nothing and hold no lock, so last_seen alone flags the first
// two as stale. Two more signals fix that, and both are already on the
// supervisor's side of the boundary:
//
//	an in-flight upstream request for the span  (proxy.Proxy.InFlight, proxy.go)
//	the container's CPU counter advancing       (the poll below)
//
// The first one is why there is no threshold tuned to model latency. A span
// with a request in flight is Live by construction, for as long as the call
// runs, with no heartbeat sidecar in any of the 19 images.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// staleAfter is the quiet period after which a span with no in-flight
	// request and no CPU movement is called stale. Same 90s as retryWindow and
	// for the same reason: a 429 with `retry-after: 60` is the longest silence
	// the transport itself can legitimately produce.
	staleAfter = 90 * time.Second

	// pollEvery bounds how late Stale and Gone can be noticed. 20 containers x
	// one one-shot stats call is a few ms against the local socket.
	pollEvery = 15 * time.Second
)

// LiveState is what glue top renders in the state column.
type LiveState uint8

const (
	Live  LiveState = iota // an upstream request is open for this span
	Busy                   // no request, but the container burned CPU recently
	Idle                   // quiet, under the threshold
	Stale                  // quiet, no request, no CPU
	Gone                   // the container is not there any more
)

func (s LiveState) String() string {
	switch s {
	case Live:
		return "live"
	case Busy:
		return "busy"
	case Idle:
		return "idle"
	case Stale:
		return "STALE"
	}
	return "GONE"
}

// Mark is the glyph glue top puts in the state column.
func (s LiveState) Mark() string {
	switch s {
	case Live:
		return "●" // ●
	case Busy:
		return "▪" // ▪
	case Idle:
		return "◦" // ◦
	case Stale:
		return "⚠" // ⚠
	}
	return "✗" // ✗
}

// Watch polls the sweep's containers for the one thing last_seen cannot see:
// whether the process is burning CPU while it says nothing.
type Watch struct {
	mu   sync.Mutex
	cpu  map[string]sample
	http *http.Client
}

type sample struct {
	ns    uint64    // cpu_stats.cpu_usage.total_usage, nanoseconds since inception
	moved time.Time // last poll at which that counter was higher than the poll before
	gone  bool
}

func NewWatch() *Watch {
	sock := dockerSocket()
	return &Watch{
		cpu: map[string]sample{},
		http: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

// dockerSocket honours DOCKER_HOST when it names a unix socket, which is how
// rootless Docker points at $XDG_RUNTIME_DIR. Any other DOCKER_HOST form is a
// different deployment than the one v0 supports and is not guessed at.
func dockerSocket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

// container is the name Net.Launch gives a span's container. There is nothing
// to map: `docker run --name glue-<span>` makes the name a pure function of the
// span, and the stats endpoint takes "ID or name of the container" (swagger
// v1.56, the `id` path parameter). So no launch bookkeeping, no id column, and
// nothing to lose when the supervisor restarts.
func container(span string) string { return "glue-" + span }

// Poll samples every span's container once. Call it on a pollEvery ticker; glue
// top reads the result, it does not trigger it.
func (w *Watch) Poll(ctx context.Context, spans []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, span := range spans {
		id := container(span)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ns, gone, err := w.cpuNanos(ctx, id)
			w.mu.Lock()
			defer w.mu.Unlock()
			prev, seen := w.cpu[id]
			switch {
			case gone:
				prev.gone = true
			case err != nil:
				// A socket hiccup is not evidence about the container. Keep the
				// previous sample; the quiet clock keeps running, so a daemon
				// that stays unreachable eventually shows every span as stale,
				// which is the truth about what we know.
			default:
				if !seen || ns > prev.ns {
					prev.moved = time.Now()
				}
				prev.ns, prev.gone = ns, false
			}
			w.cpu[id] = prev
		}(id)
	}
	wg.Wait()
}

// cpuNanos reads one container's cumulative CPU time.
//
// GET /containers/{id}/stats?stream=false&one-shot=true — verified against
// moby api/swagger.yaml v1.56 (2026-09-02):
//
//	one-shot: "Only get a single stat instead of waiting for 2 cycles.
//	           Must be used with `stream=false`."
//	cpu_stats.cpu_usage.total_usage: uint64, "Total CPU time consumed in
//	           nanoseconds (Linux)", aggregated since container inception.
//
// one-shot leaves precpu_stats zeroed, which is exactly right here: the delta
// we want spans poll intervals, not daemon read cycles, so we keep the previous
// sample ourselves. It also skips the ~1s the daemon otherwise spends waiting
// for a second cycle.
//
// The path carries no version prefix; the swagger preamble states "If you omit
// the version-prefix, the current version of the API is used."
func (w *Watch) cpuNanos(ctx context.Context, id string) (ns uint64, gone bool, err error) {
	u := "http://docker/containers/" + url.PathEscape(id) + "/stats?stream=false&one-shot=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, true, nil // container removed: the lease is over
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, &url.Error{Op: "GET", URL: u, Err: errStatus(resp.Status)}
	}
	var s struct {
		CPU struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, false, err
	}
	return s.CPU.Usage.Total, false, nil
}

type errStatus string

func (e errStatus) Error() string { return string(e) }

// State is the whole rule.
//
//	lastSeen  MAX(last_seen) over the span's events rows
//	inflight  proxy.Proxy.InFlight(span)
//
// Quiet time is measured from the LATER of the two silent-but-alive signals, so
// a span only goes Stale when the ledger, the proxy and the kernel all agree
// nothing has happened.
//
// What this still cannot see: an agent spinning in a retry loop with no network
// and no output burns CPU and reads as Busy forever. That is a real hole, and
// the honest fix is the per-task 270-minute deadline, not a cleverer poll.
func (w *Watch) State(span string, lastSeen time.Time, inflight int, now time.Time) (LiveState, time.Duration) {
	if inflight > 0 {
		return Live, 0
	}
	w.mu.Lock()
	s, seen := w.cpu[container(span)]
	w.mu.Unlock()
	if seen && s.gone {
		return Gone, now.Sub(lastSeen)
	}

	active := lastSeen
	if s.moved.After(active) {
		active = s.moved
	}
	quiet := now.Sub(active)
	switch {
	case quiet >= staleAfter:
		return Stale, quiet
	case s.moved.After(lastSeen):
		return Busy, quiet
	default:
		return Idle, quiet
	}
}
