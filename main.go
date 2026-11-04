// Command nitro-history-pruner removes old block bodies and receipts from an
// Arbitrum Nitro (or geth) chain database while keeping headers and full state.
// Run it only while the node is stopped.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"

	"github.com/qubenodeorg/nitro-history-pruner/pruner"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		chaindata    = flag.String("chaindata", "", "path to the chain database, e.g. /var/lib/arbitrum/arb1/nitro/l2chaindata (required)")
		ancient      = flag.String("ancient", "", "ancient store directory (default: <chaindata>/ancient)")
		retention    = flag.String("retention", "", "keep blocks newer than this age, e.g. 180d, 90d, 720h")
		retainBlocks = flag.Uint64("retain-blocks", 0, "keep the newest N blocks (alternative to -retention)")
		yes          = flag.Bool("yes", false, "actually prune; without it the tool only prints the plan")
		verbose      = flag.Bool("verbose", false, "show geth database logs")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -chaindata DIR (-retention AGE | -retain-blocks N) [-yes]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *chaindata == "" {
		flag.Usage()
		return errors.New("-chaindata is required")
	}
	opts := pruner.Options{RetainBlocks: *retainBlocks}
	if *retention != "" {
		d, err := parseAge(*retention)
		if err != nil {
			return err
		}
		opts.Retention = d
	}
	if (opts.Retention > 0) == (opts.RetainBlocks > 0) {
		return errors.New("set exactly one of -retention or -retain-blocks")
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, level, true)))

	dir, err := filepath.Abs(*chaindata)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("chaindata: %w", err)
	}
	ancientDir := *ancient
	if ancientDir == "" {
		ancientDir = filepath.Join(dir, "ancient")
	}

	// Opening read-write takes the database's LOCK file, so this fails if the
	// node is still running. A dry run opens read-only.
	db, err := node.OpenDatabase(node.InternalOpenOptions{
		Directory: dir,
		DatabaseOptions: node.DatabaseOptions{
			AncientsDirectory: ancientDir,
			Cache:             512,
			Handles:           512,
			ReadOnly:          !*yes,
		},
	})
	if err != nil {
		return fmt.Errorf("opening database (is the node stopped?): %w", err)
	}
	defer db.Close()

	plan, err := pruner.MakePlan(db, pruner.ResolveFreezerDir(ancientDir), opts)
	if err != nil {
		return err
	}
	printPlan(plan)
	if plan.Noop() {
		fmt.Println("\nNothing to prune: the database is already pruned to this point or beyond.")
		return nil
	}
	if !*yes {
		fmt.Println("\nDry run. Re-run with -yes to prune.")
		return nil
	}

	fmt.Println("\nPruning transaction index (this scans every lookup entry and can take a while)...")
	start := time.Now()
	if err := pruner.Execute(db, plan); err != nil {
		return err
	}
	fmt.Printf("Done in %s. New tail: block %d.\n", time.Since(start).Round(time.Second), plan.Cutoff)
	return nil
}

func printPlan(p *pruner.Plan) {
	fmt.Printf("Head block:            %d\n", p.Head)
	fmt.Printf("Blocks in ancient:     %d\n", p.Frozen)
	fmt.Printf("Current history tail:  %d\n", p.Tail)
	if p.Noop() {
		return
	}
	fmt.Printf("New history tail:      %d (%s, %s)\n", p.Cutoff, p.CutoffTime.Format(time.RFC3339), p.CutoffHash)
	fmt.Printf("Blocks pruned:         %d\n", p.Cutoff-p.Tail)
	for _, t := range p.Tables {
		fmt.Printf("  %-10s %4d files, %s\n", t.Name, t.FilesDeleted, humanBytes(t.BytesFreed))
	}
	fmt.Printf("Space freed:           %s\n", humanBytes(p.BytesFreed()))
}

// parseAge accepts Go durations plus a "d" (days) suffix.
func parseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 32)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("invalid retention %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid retention %q", s)
	}
	return d, nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
