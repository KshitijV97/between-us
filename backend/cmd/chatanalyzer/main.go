// Command chatanalyzer is the single binary for every deployment profile
// (docs/02-architecture.md §2). Subcommands:
//
//	serve     run the HTTP API, web UI and in-process workers
//	import    import a WhatsApp or Telegram export (FR-ING-001)
//	chats     list or delete imported chats (FR-LIB-001, FR-LIB-002)
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
	"syscall"

	"github.com/kshitijv/chat-analyzer/backend/internal/buildinfo"
	"github.com/kshitijv/chat-analyzer/backend/internal/config"
	"github.com/kshitijv/chat-analyzer/backend/internal/httpapi"
	"github.com/kshitijv/chat-analyzer/backend/internal/telemetry"
	"github.com/kshitijv/chat-analyzer/backend/internal/webui"
)

// Exit codes follow the BSD sysexits convention for usage errors.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
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
	case "migrate":
		return migrateCmd(stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "chatanalyzer: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: chatanalyzer <command> [flags]

Commands:
  serve     Run the HTTP API and web UI
  import    Import a WhatsApp or Telegram export (run "import -h" for flags)
  chats     List chats, or "chats delete <id>"
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
	log.Info("starting chatanalyzer", "version", bi.Version, "commit", bi.ShortCommit(), "ui_embedded", webui.Built())

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

	srv := httpapi.New(httpapi.Options{Config: cfg, Logger: log, UI: webui.FS(), Checks: checks})
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
	fmt.Fprintf(stdout, "chatanalyzer %s (commit %s, built %s, %s %s)\n",
		bi.Version, bi.ShortCommit(), orUnknown(bi.Date), bi.GoVersion, bi.Platform)
	return exitOK
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
