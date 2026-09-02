// Command glue reads a sweep's ledger from outside the sweep.
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
	csv := fs.Bool("csv", false, "table: emit CSV instead of the human table")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: glue top|table [flags]")
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
	case "table":
		nodes, err := tui.Load(db, *sweep, nil)
		if err != nil {
			log.Fatal(err)
		}
		rows, err := tui.Table(db, *sweep, nodes)
		if err != nil {
			log.Fatal(err)
		}
		if *csv {
			if err := tui.TableCSV(os.Stdout, rows); err != nil {
				log.Fatal(err)
			}
			return
		}
		for _, l := range tui.TableLines(*sweep, rows) {
			fmt.Println(l)
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}
