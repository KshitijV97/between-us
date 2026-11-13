// Package text computes per-message text features for the analytics engine:
// words and characters (M-003), emoji (M-015), terms and bigrams (M-016),
// questions (M-017), links (M-018), lexicon sentiment (M-022), affection
// (M-023) and conflict (M-024). Specification: docs/04-metrics-catalog.md §8–§9.
//
// The Analyzer is built once per chat from the chat's languages and is then
// read-only, so one instance may be shared by concurrent goroutines.
// Everything is deterministic: the same body and lexicons always give the
// same features, which keeps stored rollups reproducible.
package text

import (
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"

	"github.com/kshitijv97/between-us/backend/internal/analytics"
)

// Config selects lexicons and options.
type Config struct {
	// Languages are lexicon directories, e.g. ["en", "hi-Latn"] (§9.2).
	Languages []string
	// IncludeEmoticons counts text emoticons such as ":)" as emoji
	// (count_text_emoticons, default false).
	IncludeEmoticons bool
	// LexiconDir optionally overrides embedded lexicon files file-by-file
	// (BU_LEXICON_DIR); missing files fall back to the embedded ones.
	LexiconDir string
}

// Analyzer implements analytics.TextAnalyzer.
type Analyzer struct {
	lx        *lexicon
	emoticons bool
}

var _ analytics.TextAnalyzer = (*Analyzer)(nil)

// New loads the lexicons for cfg.
func New(cfg Config) (*Analyzer, error) {
	langs := cfg.Languages
	if len(langs) == 0 {
		langs = []string{"en"}
	}
	lx, err := loadLexicon(langs, cfg.LexiconDir)
	if err != nil {
		return nil, err
	}
	return &Analyzer{lx: lx, emoticons: cfg.IncludeEmoticons}, nil
}

// LexiconHash is a stable digest of the loaded lexicon files; a change means
// stored features are out of date (§11.1).
func (a *Analyzer) LexiconHash() string { return a.lx.hash }

// VADER-style constants (Hutto & Gilbert 2014), re-derived for this engine.
const (
	capsBoost        = 0.733 // ALL-CAPS word in a mixed-case message
	negationScale    = -0.74 // a negated valence flips and shrinks
	exclaimBoost     = 0.292 // per "!" (up to 4) in the direction of the sum
	questionBoost    = 0.18  // per "?" beyond the first (up to 3)
	compoundAlpha    = 15.0  // compound = s / sqrt(s² + alpha)
	butBeforeScale   = 0.5   // words before "but" weigh less …
	butAfterScale    = 1.5   // … and words after it more
	conflictNegative = -0.5  // M-024 signal 2
)

// urlPattern finds URLs, including bare "www." domains (M-018). Trailing
// punctuation is trimmed afterwards so "see example.com." works.
var urlPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"]+`)

// repeatedPunct is M-024 signal 4's punctuation part.
var repeatedPunct = regexp.MustCompile(`!!!|\?!|!\?|\?\?`)

// emoticonValences are used only when IncludeEmoticons is on.
var emoticonValences = map[string]float64{
	":)": 1.5, ":-)": 1.5, ":D": 2.0, ":-D": 2.0, ";)": 1.2, ";-)": 1.2, "<3": 2.5,
	":P": 1.0, ":-P": 1.0, ":p": 1.0, ":(": -1.5, ":-(": -1.5, ":'(": -2.0, ":/": -0.8,
	"xD": 2.0, "XD": 2.0,
}

// token is one word of the message in matching form.
type token struct {
	raw  string // as written (for ALL-CAPS detection)
	norm string // lower-cased, elongation-folded to 2, variant-folded
	alt  string // single-letter fold for elongated tokens ("sooo" → "so"), else ""
}

// Analyze computes the features of one message body.
func (a *Analyzer) Analyze(body string) analytics.TextFeatures {
	var f analytics.TextFeatures
	if strings.TrimSpace(body) == "" {
		return f
	}

	// Characters and emoji come from the body as written.
	f.CharCount, f.Emoji = graphemes(body, nil)

	text := prepare(body)

	// Links: each URL counts as one word and is removed before segmentation
	// so its fragments ("www", "com") are not counted again or used as terms.
	if locs := urlPattern.FindAllStringIndex(text, -1); len(locs) > 0 {
		var b strings.Builder
		last := 0
		for _, l := range locs {
			raw := strings.TrimRight(text[l[0]:l[1]], ".,;:!?)]}'\"*")
			f.LinkDomains = append(f.LinkDomains, domainOf(raw))
			b.WriteString(text[last:l[0]])
			b.WriteByte(' ')
			b.WriteString(text[l[0]+len(raw) : l[1]])
			last = l[1]
		}
		b.WriteString(text[last:])
		text = b.String()
		f.WordCount += len(locs)
	}

	toks := a.tokens(text)
	f.WordCount += len(toks)

	if a.emoticons {
		for _, w := range strings.Fields(text) {
			if _, ok := emoticonValences[w]; ok {
				f.Emoji = append(f.Emoji, w)
			}
		}
	}

	f.IsQuestion = strings.ContainsRune(text, '?') || a.startsWithInterrogative(toks)
	f.Sentiment, f.SentimentScored = a.sentiment(text, toks, f.Emoji)
	f.AffectionHits = a.affection(toks, f.Emoji)
	f.Conflict = a.conflict(text, toks, f.Sentiment, f.SentimentScored)
	f.Terms, f.Bigrams = a.terms(toks)
	return f
}

// tokens segments text into words in matching form.
func (a *Analyzer) tokens(text string) []token {
	toks := make([]token, 0, 16)
	forEachWord(text, func(w string) {
		l := lower(w)
		f2, elongated := foldElongation(l, 2)
		t := token{raw: w, norm: a.lx.variantOf(f2)}
		if elongated {
			f1, _ := foldElongation(l, 1)
			t.alt = a.lx.variantOf(f1)
		}
		toks = append(toks, t)
	})
	return toks
}

func (lx *lexicon) variantOf(s string) string {
	if v, ok := lx.variants[s]; ok {
		return v
	}
	return s
}

// domainOf returns the registrable domain (eTLD+1) of a URL, so links to
// m.youtube.com and www.youtube.com group as youtube.com while youtu.be stays
// distinct. Unparseable hosts fall back to the lower-cased host or "unknown".
func domainOf(raw string) string {
	s := raw
	if !strings.Contains(strings.ToLower(s), "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "unknown"
	}
	host := strings.ToLower(u.Hostname())
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

func (a *Analyzer) startsWithInterrogative(toks []token) bool {
	for _, q := range a.lx.interrogatives {
		if len(q) <= len(toks) && matchAt(toks, 0, q) {
			return true
		}
	}
	return false
}

// matchAt reports whether phrase tokens p occur at toks[i:].
func matchAt(toks []token, i int, p []string) bool {
	if i+len(p) > len(toks) {
		return false
	}
	for k, w := range p {
		t := toks[i+k]
		if t.norm != w && t.alt != w {
			return false
		}
	}
	return true
}

func (lx *lexicon) lookup(m map[string]float64, t token) (float64, bool) {
	if v, ok := m[t.norm]; ok {
		return v, true
	}
	if t.alt != "" {
		v, ok := m[t.alt]
		return v, ok
	}
	return 0, false
}

func (lx *lexicon) negationOf(t token) (negation, bool) {
	if n, ok := lx.negations[t.norm]; ok {
		return n, true
	}
	if t.alt != "" {
		n, ok := lx.negations[t.alt]
		return n, ok
	}
	return negation{}, false
}

// negated reports whether the word span toks[start:end] is negated. English
// negations reach up to three words forward ("not really that good"); Hindi
// negations usually follow the word ("accha nahi hai") but may precede it
// directly ("nahi accha"), so "both" negations look one word back and two
// forward. The asymmetric windows avoid "nahi yaar, bahut accha" (no man,
// very good) being read as negated.
func (lx *lexicon) negated(toks []token, start, end int) bool {
	for j := start - 1; j >= 0 && j >= start-3; j-- {
		if n, ok := lx.negationOf(toks[j]); ok && (!n.both || j == start-1) {
			return true
		}
	}
	for j := end; j < len(toks) && j < end+2; j++ {
		if n, ok := lx.negationOf(toks[j]); ok && n.both {
			return true
		}
	}
	return false
}

// sentiment computes a VADER-style compound score. Each lexicon word
// contributes its valence, boosted when it is ALL CAPS in a mixed-case message,
// scaled by up to two preceding intensifiers, flipped and damped when negated,
// and weighted around "but". Emoji valences are added as is. Exclamation and
// repeated question marks push the sum away from zero. The sum s maps to
// [-1, 1] by s/sqrt(s²+15).
func (a *Analyzer) sentiment(text string, toks []token, emoji []string) (float64, bool) {
	lx := a.lx
	capsDiff := false
	{
		upper, other := 0, 0
		for _, t := range toks {
			if isAllCaps(t.raw) {
				upper++
			} else if hasLetter(t.raw) {
				other++
			}
		}
		capsDiff = upper > 0 && other > 0
	}
	butAt := -1
	for i, t := range toks {
		if t.norm == "but" || t.norm == "lekin" || t.norm == "magar" {
			butAt = i
		}
	}

	sum, hits := 0.0, 0
	for i, t := range toks {
		v, ok := lx.lookup(lx.sentiment, t)
		if !ok || v == 0 {
			continue
		}
		hits++
		if capsDiff && isAllCaps(t.raw) {
			v += math.Copysign(capsBoost, v)
		}
		for k, decay := 1, 1.0; k <= 2 && i-k >= 0; k, decay = k+1, 0.95 {
			if m, ok := lx.lookup(lx.intensifiers, toks[i-k]); ok {
				v *= 1 + (m-1)*decay
			}
		}
		if lx.negated(toks, i, i+1) {
			v *= negationScale
		}
		if butAt >= 0 {
			switch {
			case i < butAt:
				v *= butBeforeScale
			case i > butAt:
				v *= butAfterScale
			}
		}
		sum += v
	}
	for _, e := range emoji {
		if v, ok := lx.emojiSentiment[baseEmoji(e)]; ok {
			sum += v
			hits++
		} else if v, ok := emoticonValences[e]; ok && a.emoticons {
			sum += v
			hits++
		}
	}
	if hits == 0 {
		return 0, false
	}
	if sum != 0 {
		ex := min(strings.Count(text, "!"), 4)
		sum += math.Copysign(float64(ex)*exclaimBoost, sum)
		if q := strings.Count(text, "?"); q > 1 {
			sum += math.Copysign(float64(min(q, 4)-1)*questionBoost, sum)
		}
	}
	c := sum / math.Sqrt(sum*sum+compoundAlpha)
	return math.Max(-1, math.Min(1, c)), true
}

// affection counts affection-lexicon phrase hits that are not negated, plus
// affection emoji (M-023). Longest phrases match first and matches do not
// overlap, so "i love you" is one hit, not two.
func (a *Analyzer) affection(toks []token, emoji []string) int {
	lx := a.lx
	hits := 0
	for i := 0; i < len(toks); {
		matched := 0
		for _, p := range lx.affection[toks[i].norm] {
			if matchAt(toks, i, p.tokens) {
				matched = len(p.tokens)
				break
			}
		}
		if matched == 0 {
			for _, p := range lx.affectionPfx {
				if strings.HasPrefix(toks[i].norm, p.tokens[0]) {
					matched = 1
					break
				}
			}
		}
		if matched == 0 {
			i++
			continue
		}
		if !lx.negated(toks, i, i+matched) {
			hits++
		}
		i += matched
	}
	for _, e := range emoji {
		if _, ok := lx.emojiAffection[baseEmoji(e)]; ok {
			hits++
		} else if e == "<3" && a.emoticons {
			hits++
		}
	}
	return hits
}

// conflict applies the M-024 rule: a message is conflict-flagged when one
// strong conflict phrase occurs, or at least two of these signals fire:
// (1) a conflict phrase, (2) compound sentiment ≤ -0.5, (3) shouting — ALL
// CAPS for ≥70% of at least four cased letters, (4) repeated "!!!"/"?!"
// punctuation together with negative sentiment. Requiring two weak signals
// keeps playful "whatever 😂" or "kya!!!" from being flagged.
func (a *Analyzer) conflict(text string, toks []token, compound float64, scored bool) bool {
	signals := 0
	phraseHit := false
	for i := 0; i < len(toks); i++ {
		for _, p := range a.lx.conflict[toks[i].norm] {
			if matchAt(toks, i, p.tokens) {
				if p.strong {
					return true
				}
				phraseHit = true
				break
			}
		}
	}
	if phraseHit {
		signals++
	}
	if scored && compound <= conflictNegative {
		signals++
	}
	upper, cased := 0, 0
	for _, t := range toks {
		for _, r := range t.raw {
			switch {
			case unicode.IsUpper(r):
				upper++
				cased++
			case unicode.IsLower(r):
				cased++
			}
		}
	}
	if cased >= 4 && float64(upper) >= 0.7*float64(cased) {
		signals++
	}
	if scored && compound < -0.05 && repeatedPunct.MatchString(text) {
		signals++
	}
	return signals >= 2
}

// terms returns M-016 terms and bigrams: normalised tokens without stop
// words, numbers and one-letter tokens. A bigram is two adjacent kept tokens,
// so a stop word in between breaks it and bigrams never cross messages.
func (a *Analyzer) terms(toks []token) (terms, bigrams []string) {
	kept := make([]bool, len(toks))
	for i, t := range toks {
		if a.isStop(t) || !hasLetter(t.norm) || runeCount(t.norm) < 2 {
			continue
		}
		kept[i] = true
		terms = append(terms, t.norm)
	}
	for i := 0; i+1 < len(toks); i++ {
		if kept[i] && kept[i+1] {
			bigrams = append(bigrams, toks[i].norm+" "+toks[i+1].norm)
		}
	}
	return terms, bigrams
}

func (a *Analyzer) isStop(t token) bool {
	if _, ok := a.lx.stop[t.norm]; ok {
		return true
	}
	if t.alt != "" {
		_, ok := a.lx.stop[t.alt]
		return ok
	}
	return false
}

func isAllCaps(s string) bool {
	letters := 0
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsUpper(r) {
			letters++
		}
	}
	return letters >= 2
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func runeCount(s string) int { return len([]rune(s)) }
