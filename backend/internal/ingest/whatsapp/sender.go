package whatsapp

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
)

// Sender lexicon (docs/parsers/whatsapp.md §5).
//
// "Sender: text" cannot be split on the first colon alone: names may contain
// ": " ("Dr: Alice") and bodies very often do ("Note: buy milk"). Pass 1
// counts two candidates per header (the text before the first and before the
// second ": "); pass 2 accepts a candidate only if it recurs, and prefers the
// longer one only when it recurs about as often as the shorter one.
const (
	maxSenderRunes = 100
	// maxCandidates bounds lexicon memory on huge or hostile files. Real
	// senders appear early and often, so a full map still holds them.
	maxCandidates = 50000
	// minSenderCount and minSenderShare are the §5 acceptance thresholds.
	minSenderCount = 3
	minSenderShare = 0.01
	// longCandidateShare: a two-colon candidate wins only if it accounts for at
	// least this share of the one-colon candidate's occurrences. "Dr: Alice"
	// matches on every one of Dr's lines; "Alice: Note" on a few of Alice's.
	longCandidateShare = 0.5
)

type lexicon struct {
	counts  map[string]int
	headers int
}

func newLexicon() *lexicon { return &lexicon{counts: map[string]int{}} }

// candidates returns the one- and two-colon prefixes of rest, if plausible.
func candidates(rest string) (c1, c2 string) {
	i := strings.Index(rest, ": ")
	if i <= 0 {
		return "", ""
	}
	c1 = rest[:i]
	if utf8.RuneCountInString(c1) > maxSenderRunes {
		return "", ""
	}
	// A second segment starting with '<' is a placeholder ("<attached: …>"),
	// never part of a name; without this rule a chat of only attachments
	// would turn "Alice: <attached" into a sender.
	if j := strings.Index(rest[i+2:], ": "); j > 0 && rest[i+2] != '<' {
		c2 = rest[:i+2+j]
		if utf8.RuneCountInString(c2) > maxSenderRunes {
			c2 = ""
		}
	}
	return c1, c2
}

// observe records the candidates of one header (pass 1).
func (l *lexicon) observe(rest string) {
	l.headers++
	c1, c2 := candidates(rest)
	for _, c := range []string{c1, c2} {
		if c == "" {
			continue
		}
		if _, seen := l.counts[c]; seen || len(l.counts) < maxCandidates {
			l.counts[c]++
		}
	}
}

func (l *lexicon) accepted(name string) bool {
	n := l.counts[name]
	return n >= minSenderCount || (n > 0 && float64(n) >= minSenderShare*float64(l.headers))
}

// split separates "Sender: text". ok=false means rest is a sender-less system
// line (no accepted sender prefix).
func (l *lexicon) split(rest string) (sender, text string, ok bool) {
	c1, c2 := candidates(rest)
	if c2 != "" && l.accepted(c2) && float64(l.counts[c2]) >= longCandidateShare*float64(l.counts[c1]) {
		return c2, rest[len(c2)+2:], true
	}
	if c1 != "" && l.accepted(c1) {
		return c1, rest[len(c1)+2:], true
	}
	return "", rest, false
}

// phoneLike matches a sender that is an unsaved phone number, e.g.
// "+91 98765 43210" or "(555) 010-2345".
var phoneLike = regexp.MustCompile(`^\+?[\d\s\-().]{7,}$`)

// senderKey derives the stable per-chat key for a display name: directional
// marks removed, NFC, and phone numbers reduced to "+digits" (E.164-like) so
// the same number written with different spacing maps to one participant.
func senderKey(display string) string {
	k := ingest.CleanText(display)
	k = strings.TrimSpace(k)
	if phoneLike.MatchString(k) {
		var b strings.Builder
		if strings.HasPrefix(k, "+") {
			b.WriteByte('+')
		}
		digits := 0
		for _, r := range k {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
				digits++
			}
		}
		// Only real numbers collapse to digits. A name made of punctuation
		// and spaces ("(  (") also matches phoneLike and would otherwise get
		// an empty key (found by FuzzParse).
		if digits >= 7 {
			return b.String()
		}
	}
	return k
}
