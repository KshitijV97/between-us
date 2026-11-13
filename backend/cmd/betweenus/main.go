// Command betweenus is the single binary for every deployment profile
// (docs/02-architecture.md §2). Subcommands:
//
//	serve     run the HTTP API, web UI and in-process workers
//	import    import a WhatsApp or Telegram export (FR-ING-001)
//	chats     list or delete imported chats (FR-LIB-001, FR-LIB-002)
//	metrics   show metrics for a period (FR-ANA-001…003)
//	analyze   recompute sessions, features and daily rollups
//	migrate   apply database migrations (NFR-REL-004)
//	version   print build information
//
// Configuration comes from CA_* environment variables; see .env.example.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/kshitijv97/between-us/backend/internal/analysis"
	"github.com/kshitijv97/between-us/backend/internal/buildinfo"
	"github.com/kshitijv97/between-us/backend/internal/config"
	"github.com/kshitijv97/between-us/backend/internal/httpapi"
	"github.com/kshitijv97/between-us/backend/internal/telemetry"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"
	"github.com/kshitijv97/between-us/backend/internal/webui"
)

// Exit codes follow the BSD sysexits convention for usage errors.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// defaultMemoryLimit is a soft heap limit: past it the garbage collector
// works harder instead of letting the process grow (NFR-PERF-001). It is not
// a cap; live data above it still fits. GOMEMLIMIT overrides it.
const defaultMemoryLimit = 128 << 20

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemoryLimit)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without process-global side effects, so it can be tested.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return serve(rest, stderr)
	case "version":
		return version(rest, stdout, stderr)
	case "import":
		return importCmd(rest, stdout, stderr)
	case "chats":
		return chatsCmd(rest, stdout, stderr)
	case "metrics":
		return metricsCmd(rest, stdout, stderr)
	case "analyze":
		return analyzeCmd(rest, stdout, stderr)
	case "migrate":
		return migrateCmd(stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "betweenus: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: betweenus <command> [flags]

Commands:
  serve     Run the HTTP API and web UI
  import    Import a WhatsApp or Telegram export (run "import -h" for flags)
  chats     List chats, or "chats delete <id>"
  metrics   Show a chat's metrics for a day, week, month, year or custom period
  analyze   Recompute a chat's derived data (runs automatically after import)
  migrate   Apply database migrations
  version   Print build information (-json for machine-readable output)
  help      Show this help

Configuration is read from CA_* environment variables (see .env.example).
`)
}

// serve loads configuration, starts the HTTP server and blocks until SIGINT or
// SIGTERM, then shuts down gracefully.
func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "configuration error:\n%v\n", err)
		return exitUsage
	}
	log := telemetry.NewLogger(stderr, cfg)
	bi := buildinfo.Get()
	log.Info("starting betweenus", "version", bi.Version, "commit", bi.ShortCommit(), "ui_embedded", webui.Built())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checks := map[string]httpapi.ReadinessCheck{}
	st, err := openStore(ctx, cfg)
	switch {
	case errors.Is(err, errPostgresNotYet):
		// Hosted profiles still serve health and the UI shell; readiness
		// reports the missing store so no traffic is routed to it.
		log.Warn("database not available", "reason", err.Error())
		checks["db"] = func(context.Context) error { return errPostgresNotYet }
	case err != nil:
		log.Error("opening database failed", "error", err)
		return exitError
	default:
		defer st.Close()
		if cfg.MigrateOnStart {
			if err := st.Migrate(ctx); err != nil {
				log.Error("migration failed", "error", err)
				return exitError
			}
		}
		checks["db"] = st.Ping
	}

	opts := httpapi.Options{Config: cfg, Logger: log, UI: webui.FS(), Checks: checks}
	if st != nil {
		opts.Store = st
		opts.Analysis = &analysis.Service{Store: st, LexiconDir: cfg.LexiconDir, Log: log}
		if !cfg.IsHosted() {
			t, u, err := st.EnsureLocalTenant(ctx)
			if err != nil {
				log.Error("preparing local workspace failed", "error", err)
				return exitError
			}
			opts.LocalScope = &tenancy.Scope{TenantID: t.ID, UserID: u.ID}
		}
	}
	srv := httpapi.New(opts)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server stopped with error", "error", err)
		return exitError
	}
	log.Info("stopped")
	return exitOK
}

func version(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	bi := buildinfo.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(bi); err != nil {
			return exitError
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "betweenus %s (commit %s, built %s, %s %s)\n",
		bi.Version, bi.ShortCommit(), orUnknown(bi.Date), bi.GoVersion, bi.Platform)
	return exitOK
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
