package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jacred/app"
	"jacred/filedb"
)

// `jacred maintain` — the integrity check and bucket restore, without starting
// the server.
//
// Both already exist over HTTP as /dev/findcorrupt and /dev/restorebucket, and
// that is the right way to use them day to day. What HTTP cannot cover is the
// case they are most needed in: the database is damaged badly enough that the
// process will not come up, or it is up and busy and you want to look without
// adding load. A listener is not a prerequisite for reading files.
//
// It is deliberately the same code underneath — db.UnreadableKeys,
// db.SnapshotsForKey, db.RestoreBucketFromSnapshot — not a parallel
// implementation. A second copy of the restore logic is the last thing this
// should grow: the restore is the operation that, done wrong with `cp`,
// destroys the snapshot it is restoring from.

const maintainUsage = `jacred maintain — offline FileDB integrity check and restore

  jacred maintain [--mode=report|restore] [--date=DD-MM-YYYY] [--key=<fdbKey>]

  --mode=report    scan for unreadable buckets and print what could restore
                   them. Changes nothing. The default.
  --mode=restore   restore every unreadable bucket from a snapshot, or just
                   --key if given.
  --date           restore from a chosen snapshot day instead of the newest.
  --key            act on one bucket key (as report prints it).

Run it from the directory that holds init.yaml and Data/, the same place the
server runs from. The server does not have to be stopped for --mode=report.
For --mode=restore, stop it: a bucket with unflushed changes is refused anyway,
but a running parser can write the file again underneath you.
`

// runMaintain is the `maintain` subcommand. It returns the process exit code:
// 0 clean, 1 something is still damaged, 2 bad usage.
func runMaintain(args []string) int {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mode := fs.String("mode", "report", "report|restore")
	date := fs.String("date", "", "snapshot day, DD-MM-YYYY")
	key := fs.String("key", "", "one bucket key")
	fs.Usage = func() { fmt.Fprint(os.Stderr, maintainUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch strings.ToLower(*mode) {
	case "report", "restore":
	default:
		fmt.Fprintf(os.Stderr, "maintain: unknown mode %q\n\n", *mode)
		fs.Usage()
		return 2
	}

	// The config is read only for fdbPathLevels, which decides where a key
	// maps on disk. LoadConfig *writes* a default init.yaml when the file is
	// absent, which a read-only diagnostic must not do — least of all in a
	// directory that turned out to be the wrong one — so the file is checked
	// first and the defaults are used in place rather than written out.
	cfg := app.DefaultConfig()
	if _, statErr := os.Stat("init.yaml"); statErr == nil {
		if loaded, err := app.LoadConfig("init.yaml"); err == nil {
			cfg = loaded
		} else {
			fmt.Fprintf(os.Stderr, "maintain: could not read init.yaml (%v) — using defaults\n", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "maintain: no init.yaml here — using default path layout")
	}
	if _, statErr := os.Stat(filepath.Join("Data", "fdb")); statErr != nil {
		fmt.Fprintf(os.Stderr, "maintain: no Data/fdb here (%v)\n", statErr)
		fmt.Fprintln(os.Stderr, "Run this from the directory that holds init.yaml and Data/.")
		return 2
	}

	db := filedb.New(cfg, "Data")
	// The index has to be loaded or there is nothing to scan: UnreadableKeys
	// walks the master entries, and a fresh DB has none until this runs. The
	// first live run of this command reported "clean" over two deliberately
	// corrupted buckets for exactly that reason. RebuildIndexes falls back to
	// walking the .gz files when masterDb.bz is missing or short, which is
	// also the state a damaged database is likely to be in.
	if err := db.RebuildIndexes(); err != nil {
		fmt.Fprintf(os.Stderr, "maintain: could not load the index: %v\n", err)
		return 1
	}

	keys := []string{}
	if strings.TrimSpace(*key) != "" {
		keys = []string{strings.TrimSpace(*key)}
	} else {
		fmt.Println("scanning buckets…")
		keys = db.UnreadableKeys()
		sort.Strings(keys)
	}

	if len(keys) == 0 {
		fmt.Println("clean: no unreadable buckets")
		return 0
	}

	fmt.Printf("%d unreadable bucket(s):\n", len(keys))
	restorable := 0
	for _, k := range keys {
		// A snapshot existing and a snapshot being *readable* are different
		// facts, and only the second one restores anything. Reporting the
		// first was actively misleading: measured on 2026-11-13, a live bucket
		// corrupted by an in-place write took its hard-linked snapshot down
		// with it, and this said "2 of 2 could be restored" when neither
		// could. SnapshotCandidate carries Readable for exactly this.
		var good, bad []string
		for _, c := range db.SnapshotsForKey(k) {
			if c.Readable {
				good = append(good, fmt.Sprintf("%s (%d records)", c.Date, c.Records))
			} else {
				bad = append(bad, c.Date)
			}
		}
		switch {
		case len(good) > 0:
			restorable++
			fmt.Printf("  %s\n      restorable from: %s\n", k, strings.Join(good, ", "))
			if len(bad) > 0 {
				fmt.Printf("      also damaged in: %s\n", strings.Join(bad, ", "))
			}
		case len(bad) > 0:
			// Both copies share an inode until the bucket is rewritten, so an
			// in-place write damages them together. jacred never does that —
			// writeBucket replaces by rename — but an operator or an outside
			// tool can.
			fmt.Printf("  %s\n      NO READABLE SNAPSHOT; present but damaged in: %s\n", k, strings.Join(bad, ", "))
		default:
			fmt.Printf("  %s\n      NO SNAPSHOT at all; the next save of this key rewrites it, and\n"+
				"      its records come back on the next sweep\n", k)
		}
	}

	if strings.ToLower(*mode) == "report" {
		// Reporting changes nothing, and a bulk mutation should be asked for
		// rather than assumed — the same split /dev/restorebucket makes
		// between its bare call and ?all=1.
		fmt.Printf("\n%d of %d have a readable snapshot. Re-run with --mode=restore to use it.\n", restorable, len(keys))
		if restorable < len(keys) {
			return 1
		}
		return 0
	}

	// db.RestoreAllUnreadable already does the bulk case, reporting each key
	// separately and never letting one failure stop the rest. Using it rather
	// than looping here keeps one implementation of the operation that, done
	// wrong with `cp`, destroys the snapshot it restores from.
	fmt.Println()
	failed := 0
	done := 0
	if strings.TrimSpace(*key) != "" {
		c, err := db.RestoreBucketFromSnapshot(keys[0], *date)
		if err != nil {
			failed++
			fmt.Printf("  FAILED %s: %v\n", keys[0], err)
		} else {
			done++
			fmt.Printf("  restored %s from %s (%d records)\n", keys[0], c.Date, c.Records)
		}
	} else {
		for _, res := range db.RestoreAllUnreadable(*date) {
			if res.Restored {
				done++
				fmt.Printf("  restored %s from %s (%d records)\n", res.Key, res.RestoredFrom, res.Records)
			} else {
				failed++
				fmt.Printf("  FAILED %s: %s\n", res.Key, res.Error)
			}
		}
	}
	left := db.UnreadableKeys()
	fmt.Printf("\n%d restored, %d failed, %d unreadable bucket(s) left\n", done, failed, len(left))
	if failed > 0 || len(left) > 0 {
		return 1
	}
	return 0
}
