// Command chatanalyzer is the single binary for every deployment profile
// (docs/02-architecture.md §2). Subcommands:
//
//	serve     run the HTTP API, web UI and in-process workers
//	version   print build information
//	migrate   apply database migrations          (milestone M1)
//	import    import a chat export from the CLI  (milestone M1, FR-ING-001)
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
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotReady = 69 // EX_UNAVAILABLE: command exists but is not implemented yet
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
	case "migrate", "import":
		fmt.Fprintf(stderr, "chatanalyzer %s: not implemented yet (planned for milestone M1, see docs/10-roadmap.md)\n", cmd)
		return exitNotReady
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
  version   Print build information (-json for machine-readable output)
  migrate   Apply database migrations (coming in M1)
  import    Import a WhatsApp or Telegram export (coming in M1)
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

	srv := httpapi.New(httpapi.Options{Config: cfg, Logger: log, UI: webui.FS()})
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
