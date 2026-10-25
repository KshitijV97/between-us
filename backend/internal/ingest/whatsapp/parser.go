// Package whatsapp parses WhatsApp "Export chat" text files (Android and iOS)
// into canonical ingest messages.
//
// Specification: docs/parsers/whatsapp.md. In short:
//
//   - A message starts with a header line ("31/12/2023, 21:41 - Bob: hi" or
//     "[31/12/23, 9:41:05 PM] Alice: hi") and continues until the next header.
//   - The date order (DMY, MDY, YMD) is not stated anywhere, so a first pass
//     over the file infers it from the evidence (§4) and builds a sender
//     lexicon (§5); a second pass streams the messages. Neither pass holds the
//     file in memory (FR-ING-013).
//   - Times are wall-clock without a zone; Options.Location turns them into
//     instants with explicit DST rules (§10).
//
// The package registers its parser with ingest on import.
package whatsapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/kshitijv97/chat-analyzer/backend/internal/ingest"
)

// Limits protecting memory and the database from hostile or broken files.
const (
	// maxLineBytes: longer lines are skipped (a real line is a few KiB).
	maxLineBytes = 1 << 20
	// maxBodyBytes caps one message body (§6); the rest is truncated.
	maxBodyBytes = 64 << 10
	// sampleLines caps detection to the first 400 lines (§8).
	sampleLines = 400
	// cancelCheckEvery is how often (in lines) pass 1 checks ctx.
	cancelCheckEvery = 4096
)

// Errors returned by Parse.
var (
	// ErrNoLocation: WhatsApp times have no zone, so a timezone is mandatory.
	ErrNoLocation = errors.New("whatsapp: Options.Location is required because exports carry no timezone")
	// ErrNoHeaders: the input contains no WhatsApp message header at all.
	ErrNoHeaders = errors.New("whatsapp: no message headers found")
)

// Parser implements ingest.Parser for WhatsApp text exports. It is stateless
// and safe for concurrent use.
type Parser struct{}

// New returns a WhatsApp parser.
func New() *Parser { return &Parser{} }

func init() { ingest.Register(New()) }

// Platform implements ingest.Parser.
func (*Parser) Platform() ingest.Platform { return ingest.PlatformWhatsApp }

// Detect implements ingest.Parser with the scoring of §8:
//
//	confidence = header_ratio × (1 − 0.5 × conflict_rate)
//
// header_ratio is the share of "header-looking" lines (starting with a digit
// or '[') that really parse as headers; conflict_rate is the share of headers
// that disagree with the dominant style (Android vs iOS, 12 h vs 24 h).
func (*Parser) Detect(sample []byte) ingest.Detection {
	det := ingest.Detection{Platform: ingest.PlatformWhatsApp}
	trimmed := strings.TrimLeft(string(sample), " \t\r\n\ufeff")
	if strings.HasPrefix(trimmed, "{") {
		det.Hints = []string{"input is JSON, not a WhatsApp text export"}
		return det
	}
	text := string(sample)
	// A sample cut at SampleSize may end mid-line; drop the partial line.
	if len(sample) >= ingest.SampleSize {
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i]
		}
	}

	var candidates, android, ios, twelve, twentyFour int
	for i, raw := range strings.Split(text, "\n") {
		if i >= sampleLines {
			break
		}
		norm := normalizeLine(raw)
		if norm == "" || (norm[0] != '[' && (norm[0] < '0' || norm[0] > '9')) {
			continue
		}
		candidates++
		h, ok := parseHeader(norm)
		if !ok {
			continue
		}
		if h.ios {
			ios++
		} else {
			android++
		}
		if h.mer != 0 {
			twelve++
		} else {
			twentyFour++
		}
	}
	headers := android + ios
	if headers == 0 || candidates == 0 {
		det.Hints = []string{"no WhatsApp message headers such as \"31/12/2023, 21:41 - Name: text\" were found"}
		return det
	}
	ratio := float64(headers) / float64(candidates)
	conflicts := min(android, ios) + min(twelve, twentyFour)
	det.Confidence = ratio * (1 - 0.5*float64(conflicts)/float64(headers))

	style := "android"
	if ios > android {
		style = "ios"
	}
	clock := "24h"
	if twelve > twentyFour {
		clock = "12h"
	}
	det.Variant = style + "-" + clock

	if headers < 3 {
		// §8 requires at least three headers; a shorter sample is too thin to
		// trust, so stay below MinConfidence but say why.
		det.Confidence = min(det.Confidence, 0.5)
		det.Hints = append(det.Hints, "fewer than three message headers found")
	}
	if det.Confidence < 0.8 && det.Confidence >= ingest.MinConfidence {
		det.Hints = append(det.Hints, "some lines look like headers but do not parse; the date or time format may be unusual")
	}
	return det
}

// Parse implements ingest.Parser.
func (p *Parser) Parse(ctx context.Context, src ingest.Source, opts ingest.Options, sink ingest.Sink) (ingest.Report, error) {
	rep := ingest.Report{Platform: ingest.PlatformWhatsApp, SuggestedTitle: titleFromName(src.Name())}
	if opts.Location == nil {
		return rep, ErrNoLocation
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = ingest.DefaultBatchSize
	}

	// Pass 1: date order, clock style, precision and sender lexicon.
	inf, err := infer(ctx, src)
	if err != nil {
		return rep, err
	}
	if inf.stats.headers == 0 {
		return rep, ErrNoHeaders
	}
	dec := inf.stats.decide()
	if opts.DateOrder != ingest.DateOrderAuto {
		dec = decision{order: opts.DateOrder, evidence: "supplied by the user"}
	}
	if dec.conflict {
		sink.Diagnostic(ingest.Diagnostic{Severity: ingest.SeverityWarning, Code: "date_order_conflict",
			Detail: "both day-first and month-first dates occur; the file may mix formats"})
	}
	rep.DateOrder, rep.DateOrderEvidence, rep.DateOrderAmbiguous = dec.order, dec.evidence, dec.ambiguous
	rep.TimePrecision = ingest.PrecisionMinute
	if inf.withSeconds > 0 {
		rep.TimePrecision = ingest.PrecisionSecond
	}
	rep.Variant = inf.variant() + "-" + strings.ToLower(string(dec.order))

	// Pass 2: stream messages.
	st := &stream{
		ctx: ctx, sink: sink, opts: opts, rep: &rep, lex: inf.lex, order: dec.order,
		batch: make([]ingest.Message, 0, opts.BatchSize),
	}
	if err := st.run(src); err != nil {
		return rep, err
	}
	finishReport(&rep, st.first)
	if st.suspiciousHeaders > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d lines look like message headers but have an impossible date or time (or a year outside 2000–2100); "+
				"they were kept as text of the preceding message. If this number is large, the date order may be wrong: "+
				"confirm it explicitly", st.suspiciousHeaders))
	}
	if st.truncatedBodies > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d messages were longer than 64 KiB and were truncated", st.truncatedBodies))
	}
	return rep, nil
}

// inference is the result of pass 1.
type inference struct {
	stats              *orderStats
	lex                *lexicon
	android, ios       int
	twelve, twentyFour int
	withSeconds        int
}

func (in *inference) variant() string {
	style, clock := "android", "24h"
	if in.ios > in.android {
		style = "ios"
	}
	if in.twelve > in.twentyFour {
		clock = "12h"
	}
	return style + "-" + clock
}

// infer is pass 1: it reads header lines only and keeps O(senders + days)
// state (§9 implementation notes).
func infer(ctx context.Context, src ingest.Source) (*inference, error) {
	rc, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	in := &inference{stats: newOrderStats(), lex: newLexicon()}
	lr := newLineReader(rc)
	for n := 1; ; n++ {
		if n%cancelCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		raw, tooLong, err := lr.next()
		if err == io.EOF {
			return in, nil
		}
		if err != nil {
			return nil, err
		}
		if tooLong {
			continue
		}
		h, ok := parseHeader(normalizeLine(raw))
		if !ok {
			continue
		}
		in.stats.add(h)
		in.lex.observe(h.rest)
		if h.ios {
			in.ios++
		} else {
			in.android++
		}
		if h.mer != 0 {
			in.twelve++
		} else {
			in.twentyFour++
		}
		if h.hasSec {
			in.withSeconds++
		}
	}
}

// stream is the pass-2 state machine of §9: ExpectHeader → InMessage → Emit.
type stream struct {
	ctx   context.Context
	sink  ingest.Sink
	opts  ingest.Options
	rep   *ingest.Report
	lex   *lexicon
	order ingest.DateOrder

	// Current message being assembled (nil in ExpectHeader).
	cur      *pending
	prevSent time.Time // last emitted SentAt, for the DST overlap rule
	batch    []ingest.Message
	first    *ingest.Message // copy of the first emitted message
	emitted  int

	// Silent-data-loss guards: both are surfaced as report warnings, because
	// a large count means messages were absorbed into other messages' text.
	suspiciousHeaders int // header-shaped lines kept as text (bad date/time)
	truncatedBodies   int // bodies cut at maxBodyBytes
}

// pending is a message whose continuation lines are still arriving.
type pending struct {
	line      int
	wall      time.Time
	sender    string // display name; "" for system lines
	body      strings.Builder
	truncated bool
}

func (st *stream) run(src ingest.Source) error {
	rc, err := src.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	lr := newLineReader(rc)
	for n := 1; ; n++ {
		raw, tooLong, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		st.rep.LinesRead++
		if tooLong {
			st.rep.AddSkipped(n, "line_too_long")
			st.diag(ingest.SeverityWarning, "line_too_long", n, "line exceeds 1 MiB and was skipped")
			continue
		}
		if h, ok := parseHeader(normalizeLine(raw)); ok {
			if wall, ok := st.wallTime(h); ok {
				if err := st.flush(); err != nil {
					return err
				}
				st.start(h, wall, n)
				continue
			}
			// Header-shaped but not a valid date/time under the decided
			// order: most likely a body quoting a timestamp (§6).
			st.suspiciousHeaders++
			st.diag(ingest.SeverityInfo, "suspicious_header", n, "header-like line treated as message text")
		}
		if st.cur == nil {
			// §6: text before the first header has no message to belong to.
			st.rep.AddSkipped(n, "orphan_line")
			st.diag(ingest.SeverityWarning, "orphan_line", n, "line before the first message header")
			continue
		}
		st.appendLine(strings.TrimRight(raw, "\r"), n)
	}
	if err := st.flush(); err != nil {
		return err
	}
	return st.emitBatch()
}

// wallTime builds the header's naive wall-clock time under the decided order.
func (st *stream) wallTime(h header) (time.Time, bool) {
	y, m, d, ok := resolveDate(h, st.order)
	if !ok {
		return time.Time{}, false
	}
	hour, minute, sec, ok := h.clock()
	if !ok {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(m), d, hour, minute, sec, 0, time.UTC), true
}

func (st *stream) start(h header, wall time.Time, line int) {
	p := &pending{line: line, wall: wall}
	sender, text, ok := st.lex.split(h.rest)
	if ok {
		p.sender = sender
	}
	p.body.WriteString(text)
	st.cur = p
}

func (st *stream) appendLine(s string, line int) {
	p := st.cur
	if p.truncated {
		return
	}
	if p.body.Len()+1+len(s) > maxBodyBytes {
		p.truncated = true
		st.truncatedBodies++
		st.diag(ingest.SeverityWarning, "message_truncated", line, "message body exceeds 64 KiB and was truncated")
		return
	}
	p.body.WriteByte('\n')
	p.body.WriteString(s)
}

// flush classifies the pending message and adds it to the batch (§9 Emit).
func (st *stream) flush() error {
	p := st.cur
	if p == nil {
		return nil
	}
	st.cur = nil

	m := ingest.Message{
		Platform:      ingest.PlatformWhatsApp,
		LocalTime:     p.wall,
		TimePrecision: st.rep.TimePrecision,
		SourceLine:    p.line,
	}
	sent, dst := toInstant(p.wall, st.opts.Location, st.prevSent)
	m.SentAt = sent
	switch dst {
	case dstGap:
		st.diag(ingest.SeverityInfo, "dst_gap_adjusted", p.line, "time falls in a daylight-saving gap; shifted forward")
	case dstOverlap:
		st.diag(ingest.SeverityInfo, "dst_overlap_resolved", p.line, "time occurs twice at a daylight-saving change")
	}
	if sent.After(st.prevSent) {
		st.prevSent = sent
	}

	body := ingest.CleanText(p.body.String())
	if typ, ok := attributedSystem(body); ok && p.sender != "" {
		m.Kind, m.Text, m.SystemType = ingest.KindSystem, body, typ
	} else if p.sender == "" {
		m.Kind, m.Text = ingest.KindSystem, body
		m.SystemType = systemType(m.Text)
	} else {
		m.SenderDisplay = strings.TrimSpace(ingest.CleanText(p.sender))
		m.SenderKey = senderKey(p.sender)
		classify(&m, p.body.String())
	}

	if st.emitted == 0 {
		first := m
		st.first = &first
	}
	st.emitted++
	st.rep.Observe(&m)
	st.batch = append(st.batch, m)
	if len(st.batch) >= st.opts.BatchSize {
		return st.emitBatch()
	}
	return nil
}

func (st *stream) emitBatch() error {
	if len(st.batch) == 0 {
		return nil
	}
	// Cancellation is honoured between batches (contract).
	if err := st.ctx.Err(); err != nil {
		return err
	}
	if err := st.sink.Batch(st.ctx, st.batch); err != nil {
		return fmt.Errorf("whatsapp: sink: %w", err)
	}
	// A fresh slice: the sink may retain the one it was given.
	st.batch = make([]ingest.Message, 0, st.opts.BatchSize)
	return nil
}

func (st *stream) diag(sev ingest.Severity, code string, line int, detail string) {
	st.sink.Diagnostic(ingest.Diagnostic{Severity: sev, Code: code, Line: line, Detail: detail})
}

// finishReport adds warnings that need the whole file.
func finishReport(rep *ingest.Report, first *ingest.Message) {
	// §1: exports normally open with the encryption notice or the group's
	// creation; anything else suggests WhatsApp's export cap cut the start.
	if first != nil && first.SystemType != "encryption_notice" && first.SystemType != "group_created" {
		rep.Warnings = append(rep.Warnings, "the export does not start with the encryption notice and may be truncated at the start")
	}
	if n := len(rep.Participants); n > 2 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d senders detected; choose the two participants to analyse (FR-ING-012)", n))
	}
	if rep.DateOrderAmbiguous {
		rep.Warnings = append(rep.Warnings, "the date order could not be determined with certainty; please confirm it")
	}
}

// titleFromName derives a chat title from the export's file name
// ("WhatsApp Chat with Bob.txt" → "Bob"). It is only a hint (§1).
func titleFromName(name string) string {
	base := strings.TrimSuffix(path.Base(name), path.Ext(name))
	for _, prefix := range []string{"WhatsApp Chat with ", "WhatsApp Chat - ", "WhatsApp-Chat mit ", "Chat de WhatsApp con "} {
		if t, ok := strings.CutPrefix(base, prefix); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// lineReader yields lines without their trailing newline, never buffering more
// than maxLineBytes of a single line.
type lineReader struct {
	r   *bufio.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next line. tooLong reports a line over maxLineBytes, whose
// content is discarded. err is io.EOF after the last line.
func (lr *lineReader) next() (line string, tooLong bool, err error) {
	lr.buf = lr.buf[:0]
	for {
		chunk, err := lr.r.ReadSlice('\n')
		if !tooLong {
			if len(lr.buf)+len(chunk) > maxLineBytes {
				tooLong, lr.buf = true, lr.buf[:0]
			} else {
				lr.buf = append(lr.buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF:
			if len(lr.buf) == 0 && !tooLong {
				return "", false, io.EOF
			}
			return string(lr.buf), tooLong, nil
		case err != nil:
			return "", false, err
		}
		return strings.TrimSuffix(string(lr.buf), "\n"), tooLong, nil
	}
}
