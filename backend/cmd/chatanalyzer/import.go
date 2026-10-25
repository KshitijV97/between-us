package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv/chat-analyzer/backend/internal/importer"
	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
	"github.com/kshitijv/chat-analyzer/backend/internal/ingest/archive"
	"github.com/kshitijv/chat-analyzer/backend/internal/store"

	// Register parsers with the ingest registry.
	_ "github.com/kshitijv/chat-analyzer/backend/internal/ingest/telegram"
	_ "github.com/kshitijv/chat-analyzer/backend/internal/ingest/whatsapp"
)

// importUsage documents `chatanalyzer import` (FR-ING-001).
const importUsage = `Usage: chatanalyzer import [flags] <export file>

Imports a WhatsApp export (.txt or .zip) or a Telegram Desktop export (result.json,
or a zip of the export folder). The file itself is not kept; only messages are stored.

Flags:
`

// importCmd implements `chatanalyzer import`.
func importCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, importUsage); fs.PrintDefaults() }
	tz := fs.String("tz", "", "IANA timezone of the phone that exported the chat (default: this computer's)")
	chatID := fs.String("chat", "", "add to an existing chat (ID from `chatanalyzer chats`) instead of creating one")
	title := fs.String("title", "", "title for a new chat (default: from the export)")
	me := fs.String("me", "", "sender key that is you (see the participant list from -dry-run)")
	participants := fs.String("participants", "", "comma-separated sender keys to analyse when the export has more than two senders")
	dateOrder := fs.String("date-order", "", "DMY, MDY or YMD when automatic date-order detection is ambiguous")
	tgChat := fs.String("telegram-chat", "", "chat ID to import from a Telegram full-account export")
	entry := fs.String("entry", "", "file inside the zip to import when it contains several candidates")
	dryRun := fs.Bool("dry-run", false, "only show what would be imported; write nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	path := fs.Arg(0)

	order := ingest.DateOrder(strings.ToUpper(*dateOrder))
	switch order {
	case ingest.DateOrderAuto, ingest.DateOrderDMY, ingest.DateOrderMDY, ingest.DateOrderYMD:
	default:
		fmt.Fprintf(stderr, "-date-order must be DMY, MDY or YMD\n")
		return exitUsage
	}
	var target uuid.UUID
	if *chatID != "" {
		id, err := uuid.Parse(*chatID)
		if err != nil {
			fmt.Fprintf(stderr, "-chat: %v\n", err)
			return exitUsage
		}
		target = id
	}
	zone := *tz
	if zone == "" {
		zone = systemTimezone()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	up, err := openUpload(path, *entry)
	if err != nil {
		fmt.Fprintln(stderr, "import:", err)
		return exitError
	}
	defer up.Close()

	a, err := openApp(ctx, stderr, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer a.close()
	ctx, err = a.localScope(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	im := &importer.Importer{Store: a.store, Log: a.log}
	preq := importer.PreviewRequest{Source: up.Source, Timezone: zone, DateOrder: order, SelectedChatID: *tgChat}
	if *dryRun {
		pv, err := im.Preview(ctx, preq)
		if err != nil {
			printImportError(stderr, err, pv.Detection)
			return exitError
		}
		if *asJSON {
			return printJSON(stdout, map[string]any{"detection": pv.Detection, "report": pv.Report, "media": up.MediaCounts})
		}
		printPreview(stdout, pv, up, zone)
		return exitOK
	}

	sum, size, err := fileDigest(path)
	if err != nil {
		fmt.Fprintln(stderr, "import:", err)
		return exitError
	}
	var keep []string
	if *participants != "" {
		for _, k := range strings.Split(*participants, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keep = append(keep, k)
			}
		}
	}
	if *title == "" && up.IsZip {
		// Inside a zip the chat file is "_chat.txt"; the zip's own name
		// carries the contact ("WhatsApp Chat - Bob.zip").
		*title = titleFromFileName(filepath.Base(path))
	}
	res, err := im.Run(ctx, importer.Request{
		PreviewRequest: preq, ChatID: target, Title: *title, Participants: keep, MeKey: *me,
		FileName: filepath.Base(path), FileSize: size, FileSHA256: sum, MediaCounts: up.MediaCounts,
	})
	if errors.Is(err, importer.ErrAlreadyImported) {
		// Re-uploading the identical file is a no-op, not a failure.
		if *asJSON {
			return printJSON(stdout, map[string]any{"chat_id": res.Chat.ID, "inserted": 0, "already_imported": true})
		}
		fmt.Fprintln(stdout, "Nothing to do: this exact file was already imported into this chat.")
		return exitOK
	}
	if err != nil {
		printImportError(stderr, err, ingest.Detection{})
		if errors.Is(err, importer.ErrTooManyParticipants) {
			printParticipants(stderr, res.Report.Participants)
		}
		return exitError
	}
	if *asJSON {
		return printJSON(stdout, map[string]any{
			"chat_id": res.Chat.ID, "import_id": res.Import.ID, "inserted": res.Inserted,
			"duplicates": res.Duplicates, "filtered": res.Filtered, "report": res.Report, "media": up.MediaCounts,
		})
	}
	printResult(stdout, res, up, zone)
	return exitOK
}

// openUpload opens a plain file or zip with the default safety limits.
func openUpload(path, entry string) (*archive.Upload, error) {
	if entry != "" {
		return archive.OpenEntry(path, entry, archive.DefaultLimits())
	}
	up, err := archive.Open(path, archive.DefaultLimits())
	var amb *archive.AmbiguousEntryError
	if errors.As(err, &amb) {
		return nil, fmt.Errorf("%w; pass one of these with -entry: %s", err, strings.Join(amb.Candidates, ", "))
	}
	return up, err
}

// titleFromFileName derives a chat title from an export's file name, e.g.
// "WhatsApp Chat with Bob.zip" or "WhatsApp Chat - Bob.zip" → "Bob". It returns
// "" when the name carries no contact.
func titleFromFileName(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	for _, prefix := range []string{"WhatsApp Chat with ", "WhatsApp Chat - ", "WhatsApp Chat "} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// fileDigest hashes the uploaded file so an identical re-upload into the same
// chat is recognised without re-processing.
func fileDigest(path string) ([]byte, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- the user names the file to import
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

// systemTimezone returns this computer's IANA timezone name, falling back to
// UTC. WhatsApp times are interpreted in it unless -tz is given.
func systemTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

func printImportError(w io.Writer, err error, det ingest.Detection) {
	fmt.Fprintln(w, "import failed:", err)
	if errors.Is(err, ingest.ErrUnrecognised) {
		for _, h := range det.Hints {
			fmt.Fprintln(w, "  hint:", h)
		}
		fmt.Fprintln(w, "  Supported: WhatsApp .txt/.zip exports and Telegram Desktop JSON exports.")
	}
}

func printPreview(w io.Writer, pv importer.Preview, up *archive.Upload, zone string) {
	fmt.Fprintf(w, "Detected     %s (%s, confidence %.2f)\n", pv.Detection.Platform, pv.Detection.Variant, pv.Detection.Confidence)
	printReport(w, pv.Report, up, zone)
	fmt.Fprintln(w, "\nDry run: nothing was written. Re-run without -dry-run to import.")
}

func printResult(w io.Writer, res importer.Result, up *archive.Upload, zone string) {
	fmt.Fprintf(w, "Chat         %s  (id %s)\n", res.Chat.Title, res.Chat.ID)
	printReport(w, res.Report, up, zone)
	fmt.Fprintf(w, "Imported     %d new messages, %d already present", res.Inserted, res.Duplicates)
	if res.Filtered > 0 {
		fmt.Fprintf(w, ", %d from other senders skipped", res.Filtered)
	}
	fmt.Fprintf(w, "\nChat total   %d messages\n", res.Chat.MessageCount)
}

func printReport(w io.Writer, r ingest.Report, up *archive.Upload, zone string) {
	if r.FirstAt != nil && r.LastAt != nil {
		loc, _ := time.LoadLocation(zone)
		if loc == nil {
			loc = time.UTC
		}
		fmt.Fprintf(w, "Period       %s → %s (%s)\n", r.FirstAt.In(loc).Format(time.DateOnly), r.LastAt.In(loc).Format(time.DateOnly), zone)
	}
	if r.DateOrder != "" {
		amb := ""
		if r.DateOrderAmbiguous {
			amb = "  ⚠ ambiguous, confirm with -date-order"
		}
		fmt.Fprintf(w, "Date order   %s%s\n", r.DateOrder, amb)
	}
	fmt.Fprintf(w, "Messages     %d (+%d system lines)\n", r.Messages, r.SystemLines)
	printParticipants(w, r.Participants)
	if len(up.MediaCounts) > 0 {
		var parts []string
		for t, n := range up.MediaCounts {
			parts = append(parts, fmt.Sprintf("%s %d", t, n))
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "Media files  %s (counted, not stored)\n", strings.Join(parts, ", "))
	}
	if len(r.Skipped) > 0 {
		var parts []string
		for reason, n := range r.Skipped {
			parts = append(parts, fmt.Sprintf("%s: %d", reason, n))
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "Skipped      %s\n", strings.Join(parts, ", "))
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "Warning      %s\n", warn)
	}
}

func printParticipants(w io.Writer, ps []ingest.ParticipantSummary) {
	fmt.Fprintln(w, "Participants")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range ps {
		fmt.Fprintf(tw, "  %s\t%d messages\tkey: %s\n", p.Display, p.MessageCount, p.Key)
	}
	_ = tw.Flush() // terminal output; nothing useful to do on failure
}

func printJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return exitError
	}
	return exitOK
}

// chatsCmd implements `chatanalyzer chats [list|delete <id>]`.
func chatsCmd(args []string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	a, err := openApp(ctx, stderr, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer a.close()
	ctx, err = a.localScope(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	switch sub {
	case "list":
		chats, err := a.store.ListChats(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if len(chats) == 0 {
			fmt.Fprintln(stdout, "No chats yet. Import one with: chatanalyzer import <export file>")
			return exitOK
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTITLE\tPLATFORM\tMESSAGES\tPERIOD")
		for _, c := range chats {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", c.ID, c.Title, c.Platform, c.MessageCount, period(c))
		}
		if err := tw.Flush(); err != nil {
			return exitError
		}
		return exitOK
	case "delete":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: chatanalyzer chats delete <chat id>")
			return exitUsage
		}
		id, err := uuid.Parse(args[0])
		if err != nil {
			fmt.Fprintln(stderr, "invalid chat id:", err)
			return exitUsage
		}
		if err := a.store.DeleteChat(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintln(stderr, "no such chat")
				return exitError
			}
			fmt.Fprintln(stderr, err)
			return exitError
		}
		fmt.Fprintln(stdout, "Deleted chat", id, "and all of its messages.")
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown chats subcommand %q (use list or delete)\n", sub)
		return exitUsage
	}
}

func period(c store.Chat) string {
	if c.FirstMessageAt == nil || c.LastMessageAt == nil {
		return "-"
	}
	loc, err := time.LoadLocation(c.AnalysisTZ)
	if err != nil {
		loc = time.UTC
	}
	return c.FirstMessageAt.In(loc).Format(time.DateOnly) + " → " + c.LastMessageAt.In(loc).Format(time.DateOnly)
}

// migrateCmd implements `chatanalyzer migrate` (NFR-REL-004).
func migrateCmd(stderr io.Writer) int {
	a, err := openApp(context.Background(), stderr, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer a.close()
	fmt.Fprintln(stderr, "migrations applied")
	return exitOK
}
