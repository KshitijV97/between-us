// Package telegram parses Telegram Desktop "Machine-readable JSON" exports
// (result.json) into canonical messages. Specification:
// docs/parsers/telegram.md.
//
// Memory is bounded regardless of export size (FR-ING-013): the file is walked
// token by token with encoding/json's Decoder and only one message object is
// decoded at a time (spec §4). Both single-chat exports and full-account
// exports (chats.list) are supported; in the latter, only the chat chosen via
// Options.SelectedChatID is emitted and every other chat is skipped token by
// token without being stored (PRV-004).
package telegram

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// Variants reported in Detection and Report.
const (
	VariantSingle  = "telegram-json-single"
	VariantAccount = "telegram-json-account"
)

// maxDepth bounds JSON nesting while skipping values (docs/08-security-privacy.md §5).
const maxDepth = 64

// Errors returned by Parse. Callers use errors.Is / errors.As.
var (
	// ErrInvalidJSON wraps malformed or truncated input. The Report returned
	// alongside describes what was read before the failure.
	ErrInvalidJSON = errors.New("telegram: invalid or truncated JSON")
	// ErrNotTelegramExport means the JSON has no messages array or chat list.
	ErrNotTelegramExport = errors.New("telegram: not a Telegram Desktop export")
	// ErrGroupChat marks group exports, which take the FR-ING-012 path.
	ErrGroupChat = errors.New("telegram: group chat")
	// ErrUnsupportedChatType marks channels, bots and saved messages.
	ErrUnsupportedChatType = errors.New("telegram: unsupported chat type")
	// ErrChatNotFound means SelectedChatID is not in the account export.
	ErrChatNotFound = errors.New("telegram: selected chat not found in export")
	// ErrChatSelectionRequired is wrapped by *ChatSelectionError.
	ErrChatSelectionRequired = errors.New("telegram: full account export needs a chat selection")
)

// ChatTypeError reports a chat type that is not a two-person chat.
type ChatTypeError struct {
	Type string
}

func (e *ChatTypeError) Error() string {
	return fmt.Sprintf("telegram: chat type %q is not a one-to-one chat; only personal_chat exports can be analysed", e.Type)
}

// Unwrap classifies the error as ErrGroupChat or ErrUnsupportedChatType.
func (e *ChatTypeError) Unwrap() error {
	if isGroup(e.Type) {
		return ErrGroupChat
	}
	return ErrUnsupportedChatType
}

// ChatSummary describes one personal chat in a full-account export so the user
// can pick it. Name is user data shown in the UI, never logged.
type ChatSummary struct {
	ID           string
	Name         string
	MessageCount int
}

// ChatSelectionError is returned when a full-account export is parsed without
// Options.SelectedChatID. It lists the personal chats available.
type ChatSelectionError struct {
	Chats []ChatSummary
}

func (e *ChatSelectionError) Error() string {
	return fmt.Sprintf("telegram: full account export contains %d personal chats; choose one with SelectedChatID", len(e.Chats))
}

// Unwrap returns ErrChatSelectionRequired.
func (e *ChatSelectionError) Unwrap() error { return ErrChatSelectionRequired }

// Parser implements ingest.Parser for Telegram Desktop JSON exports.
type Parser struct{}

func init() { ingest.Register(Parser{}) }

// Platform implements ingest.Parser.
func (Parser) Platform() ingest.Platform { return ingest.PlatformTelegram }

// Parse implements ingest.Parser. It streams src and emits messages to sink in
// batches. On failure it still returns the Report accumulated so far.
func (Parser) Parse(ctx context.Context, src ingest.Source, opts ingest.Options, sink ingest.Sink) (ingest.Report, error) {
	rep := ingest.Report{
		Platform:      ingest.PlatformTelegram,
		Variant:       VariantSingle,
		TimePrecision: ingest.PrecisionSecond,
	}
	rc, err := src.Open()
	if err != nil {
		return rep, err
	}
	defer rc.Close()

	p := &parse{
		ctx:   ctx,
		opts:  opts,
		sink:  sink,
		rep:   &rep,
		dec:   json.NewDecoder(skipBOM(bufio.NewReaderSize(rc, 1<<20))),
		clock: map[time.Duration]int{},
	}
	if p.opts.BatchSize <= 0 {
		p.opts.BatchSize = ingest.DefaultBatchSize
	}
	err = p.top()
	p.finish()
	return rep, err
}

// parse holds the state of one Parse call.
type parse struct {
	ctx  context.Context
	opts ingest.Options
	sink ingest.Sink
	rep  *ingest.Report
	dec  *json.Decoder

	batch      []ingest.Message
	lastSentAt time.Time
	outOfOrder int
	truncated  int

	// clock counts LocalTime-SentAt offsets to suggest the exporter's
	// timezone; misaligned counts offsets that are not whole quarter hours.
	clock         map[time.Duration]int
	misaligned    int
	firstMisalign int
}

// chatHeader collects the top-level fields of one chat object.
type chatHeader struct {
	name, typ, id string
}

// top walks the root object of a single-chat or full-account export.
func (p *parse) top() error {
	if err := p.expectDelim('{'); err != nil {
		return err
	}
	var hdr chatHeader
	found := false
	for p.dec.More() {
		key, err := p.key()
		if err != nil {
			return err
		}
		switch key {
		case "name", "type", "id":
			if err := p.headerField(&hdr, key); err != nil {
				return err
			}
		case "messages":
			found = true
			if err := checkType(hdr.typ); err != nil {
				return err
			}
			p.rep.SuggestedTitle = hdr.name
			if err := p.messages(); err != nil {
				return err
			}
		case "chats":
			found = true
			p.rep.Variant = VariantAccount
			if err := p.accountChats(); err != nil {
				return err
			}
		default:
			if err := skipValue(p.dec); err != nil {
				return err
			}
		}
	}
	if !found {
		return ErrNotTelegramExport
	}
	if p.rep.Variant == VariantSingle {
		// The chat type may follow the messages array in hand-edited files.
		if err := checkType(hdr.typ); err != nil {
			return err
		}
	}
	return nil
}

// headerField decodes name/type/id into hdr.
func (p *parse) headerField(hdr *chatHeader, key string) error {
	var v any
	if err := p.dec.Decode(&v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	s := ""
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		s = x.String()
	}
	switch key {
	case "name":
		hdr.name = s
	case "type":
		hdr.typ = s
	case "id":
		hdr.id = s
	}
	return nil
}

// accountChats walks {"chats": {"list": [ {chat}, ... ]}} of a full-account
// export. Without a selection it only summarises personal chats; with one it
// parses the selected chat and skips the rest token by token.
func (p *parse) accountChats() error {
	if err := p.expectDelim('{'); err != nil {
		return err
	}
	var summaries []ChatSummary
	selected := false
	for p.dec.More() {
		key, err := p.key()
		if err != nil {
			return err
		}
		if key != "list" {
			if err := skipValue(p.dec); err != nil {
				return err
			}
			continue
		}
		if err := p.expectDelim('['); err != nil {
			return err
		}
		for p.dec.More() {
			sum, matched, err := p.accountChat()
			if err != nil {
				return err
			}
			if matched {
				selected = true
			}
			if sum.ID != "" {
				summaries = append(summaries, sum)
			}
		}
		if err := p.expectDelim(']'); err != nil {
			return err
		}
	}
	if err := p.expectDelim('}'); err != nil {
		return err
	}
	switch {
	case p.opts.SelectedChatID == "":
		return &ChatSelectionError{Chats: summaries}
	case !selected:
		return ErrChatNotFound
	}
	return nil
}

// accountChat handles one element of chats.list. It returns a summary for
// personal chats and whether this was the selected chat.
func (p *parse) accountChat() (ChatSummary, bool, error) {
	if err := p.expectDelim('{'); err != nil {
		return ChatSummary{}, false, err
	}
	var hdr chatHeader
	count := 0
	matched := false
	for p.dec.More() {
		key, err := p.key()
		if err != nil {
			return ChatSummary{}, false, err
		}
		switch key {
		case "name", "type", "id":
			if err := p.headerField(&hdr, key); err != nil {
				return ChatSummary{}, false, err
			}
		case "messages":
			if p.opts.SelectedChatID != "" && hdr.id == p.opts.SelectedChatID {
				matched = true
				if err := checkType(hdr.typ); err != nil {
					return ChatSummary{}, false, err
				}
				p.rep.SuggestedTitle = hdr.name
				if err := p.messages(); err != nil {
					return ChatSummary{}, false, err
				}
				continue
			}
			n, err := countAndSkipArray(p.dec)
			if err != nil {
				return ChatSummary{}, false, err
			}
			count = n
		default:
			if err := skipValue(p.dec); err != nil {
				return ChatSummary{}, false, err
			}
		}
	}
	if err := p.expectDelim('}'); err != nil {
		return ChatSummary{}, false, err
	}
	if hdr.typ != "personal_chat" {
		return ChatSummary{}, matched, nil
	}
	return ChatSummary{ID: hdr.id, Name: hdr.name, MessageCount: count}, matched, nil
}

// messages streams one "messages" array to the sink.
func (p *parse) messages() error {
	if err := p.expectDelim('['); err != nil {
		return err
	}
	for p.dec.More() {
		var r rawMessage
		if err := p.dec.Decode(&r); err != nil {
			p.rep.LinesRead++
			p.sink.Diagnostic(ingest.Diagnostic{
				Severity: ingest.SeverityError, Code: "bad_message_json", Line: p.rep.LinesRead,
				Detail: fmt.Sprintf("JSON decoding stopped at byte offset %d", p.dec.InputOffset()),
			})
			// Messages already read are handed over so the importer can offer
			// a partial import; by default it rolls them back (spec §5).
			if ferr := p.flush(); ferr != nil {
				return ferr
			}
			return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
		p.rep.LinesRead++
		if err := p.add(&r, p.rep.LinesRead); err != nil {
			return err
		}
	}
	if err := p.flush(); err != nil {
		return err
	}
	return p.expectDelim(']')
}

// add maps one raw message and appends it to the current batch.
func (p *parse) add(r *rawMessage, idx int) error {
	res := mapMessage(r)
	if res.skip != "" {
		p.rep.AddSkipped(idx, res.skip)
		return nil
	}
	m := res.msg
	m.SourceLine = idx
	if res.truncated {
		p.truncated++
		p.sink.Diagnostic(ingest.Diagnostic{
			Severity: ingest.SeverityWarning, Code: "message_truncated", Line: idx,
			Detail: "message body longer than 64 KiB was truncated",
		})
	}
	if res.hasLocal {
		if res.clockOffset%(15*time.Minute) != 0 {
			if p.misaligned == 0 {
				p.firstMisalign = idx
			}
			p.misaligned++
		} else {
			p.clock[res.clockOffset]++
		}
	}
	if m.SentAt.Before(p.lastSentAt) {
		p.outOfOrder++
	} else {
		p.lastSentAt = m.SentAt
	}
	p.rep.Observe(&m)
	p.batch = append(p.batch, m)
	if len(p.batch) >= p.opts.BatchSize {
		return p.flush()
	}
	return nil
}

// flush sorts and hands the current batch to the sink, then checks for
// cancellation. A fresh slice is allocated each time because the sink may keep
// the one it was given.
func (p *parse) flush() error {
	if len(p.batch) == 0 {
		return p.ctx.Err()
	}
	b := p.batch
	p.batch = make([]ingest.Message, 0, p.opts.BatchSize)
	// Exports are chronological, but edits and imports can disorder a few
	// items; sort within the batch by (SentAt, numeric id) (spec §5).
	sort.SliceStable(b, func(i, j int) bool {
		if !b[i].SentAt.Equal(b[j].SentAt) {
			return b[i].SentAt.Before(b[j].SentAt)
		}
		return idLess(b[i].ExternalID, b[j].ExternalID)
	})
	if err := p.sink.Batch(p.ctx, b); err != nil {
		return err
	}
	return p.ctx.Err()
}

// finish adds end-of-parse diagnostics and the timezone suggestion.
func (p *parse) finish() {
	if p.misaligned > 0 {
		p.sink.Diagnostic(ingest.Diagnostic{
			Severity: ingest.SeverityWarning, Code: "clock_mismatch", Line: p.firstMisalign,
			Detail: fmt.Sprintf("%d messages have local and UTC times that differ by a non-timezone amount; UTC times are used", p.misaligned),
		})
	}
	if p.outOfOrder > 0 {
		p.sink.Diagnostic(ingest.Diagnostic{
			Severity: ingest.SeverityInfo, Code: "out_of_order",
			Detail: fmt.Sprintf("%d messages were out of chronological order", p.outOfOrder),
		})
	}
	var best time.Duration
	bestN := 0
	for off, n := range p.clock {
		if n > bestN || (n == bestN && off < best) {
			best, bestN = off, n
		}
	}
	if bestN > 0 {
		p.rep.Warnings = append(p.rep.Warnings,
			"Export times suggest the exporting computer used UTC"+formatOffset(best)+"; use a timezone with that offset for daily statistics.")
	}
}

// key reads an object key.
func (p *parse) key() (string, error) {
	tok, err := p.dec.Token()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	k, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("%w: expected object key, got %v", ErrInvalidJSON, tok)
	}
	return k, nil
}

func (p *parse) expectDelim(want json.Delim) error {
	tok, err := p.dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		if want == '{' && p.rep.LinesRead == 0 {
			return ErrNotTelegramExport
		}
		return fmt.Errorf("%w: expected %q, got %v", ErrInvalidJSON, want, tok)
	}
	return nil
}

// skipValue consumes one JSON value of any shape without materialising it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return fmt.Errorf("%w: nesting deeper than %d", ErrInvalidJSON, maxDepth)
				}
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// countAndSkipArray skips an array value and returns its element count.
func countAndSkipArray(dec *json.Decoder) (int, error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return 0, fmt.Errorf("%w: messages is not an array", ErrInvalidJSON)
	}
	n := 0
	for dec.More() {
		if err := skipValue(dec); err != nil {
			return n, err
		}
		n++
	}
	_, err = dec.Token() // ']'
	if err != nil {
		return n, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return n, nil
}

// checkType accepts personal chats (and an absent type, as in some older
// exports) and rejects everything else.
func checkType(t string) error {
	if t == "" || t == "personal_chat" {
		return nil
	}
	return &ChatTypeError{Type: t}
}

func isGroup(t string) bool {
	switch t {
	case "private_group", "private_supergroup", "public_supergroup":
		return true
	}
	return false
}

// idLess orders numeric message IDs numerically, falling back to strings.
func idLess(a, b string) bool {
	ai, errA := strconv.ParseInt(a, 10, 64)
	bi, errB := strconv.ParseInt(b, 10, 64)
	if errA == nil && errB == nil {
		return ai < bi
	}
	return a < b
}

// skipBOM drops a UTF-8 byte order mark, which editors sometimes add.
func skipBOM(r *bufio.Reader) io.Reader {
	if b, err := r.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = r.Discard(3)
	}
	return r
}
