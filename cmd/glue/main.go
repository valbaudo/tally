// Command glue reads a sweep's ledger from outside the sweep.
//
// It is `glue top` and nothing else. There used to be a `glue table` that
// rendered a submission file; it rendered ONE benchmark's submission file, out
// of six hardcoded fact keys, which made the monitoring binary an authority on
// what a harness is doing. Reports are the harness's job — it has Outcomes()
// and it knows what its own facts mean. This binary shows spend and liveness,
// which are true of every harness.
//
// It never writes. It opens the same SQLite file the supervisor is writing,
// read-only, which is safe and lock-free because the ledger is WAL and
// append-only: a reader never blocks the writer and the writer never blocks a
// reader. That is also why this is a separate binary rather than a flag on the
// supervisor — a monitor you have to restart the thing you are monitoring to
// use is not a monitor.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/valbaudo/dawn/internal/tui"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("glue: ")
	fs := flag.NewFlagSet("glue", flag.ExitOnError)
	dbPath := fs.String("db", "glue.db", "ledger path")
	sweep := fs.String("sweep", "", "sweep id")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: glue top [flags]")
		fs.PrintDefaults()
	}
	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs.Parse(os.Args[2:])
	if *sweep == "" {
		log.Fatal("-sweep is required")
	}

	db, err := sql.Open("sqlite3", *dbPath+"?mode=ro&_busy_timeout=10000")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// caps is nil out of process: a pool ceiling belongs to a pool, and only
	// the supervisor knows which span was handed which pool. Meters render
	// blank rather than wrong. The supervisor's own Sweep.Report has them.
	switch cmd {
	case "top":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		base := tui.Frame{Sweep: *sweep, Started: time.Now()}
		if err := tui.RunTop(ctx, db, nil, tui.NewWatch(), base, nil, os.Stdout); err != nil {
			log.Fatal(err)
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}
