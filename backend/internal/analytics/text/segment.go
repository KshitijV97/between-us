package text

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

// Special code points used by emoji sequences (§9.1).
const (
	variationSelector16 = '\uFE0F' // emoji presentation; ignored for identity
	combiningKeycap     = '\u20E3'
)

// Typographic apostrophes (U+2019, U+2018, U+02BC) are mapped to ASCII so a
// curly "don’t" matches "don't".
var apostrophes = strings.NewReplacer("\u2019", "'", "\u2018", "'", "\u02BC", "'")

// prepare returns the matching form of a body: NFKC (folds full-width "？"
// to "?" and compatibility characters to their plain form) with ASCII
// apostrophes. Character and emoji counts are taken from the original body,
// not from this form, so NFKC never changes what the user sees counted.
func prepare(s string) string {
	s = apostrophes.Replace(s)
	if !norm.NFKC.IsNormalString(s) {
		s = norm.NFKC.String(s)
	}
	return s
}

// forEachWord calls fn for every UAX #29 word segment that counts as a word:
// it contains a letter or digit and is not an emoji cluster (a keycap such as
// digit + U+FE0F + U+20E3, contains a digit but is an emoji, M-003/M-015). UAX #29 keeps
// contractions ("don't") and decimals ("3.5") together, and splits CJK
// ideographs individually, which is the documented approximation for scripts
// without spaces.
func forEachWord(s string, fn func(w string)) {
	state := -1
	var w string
	for len(s) > 0 {
		w, s, state = uniseg.FirstWordInString(s, state)
		if hasLetterOrDigit(w) && !isEmojiCluster(w) {
			fn(w)
		}
	}
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// lower lower-cases a token. Locale-specific casing (Turkish dotted i) is not
// applied; chats in such locales are rare in this product's target languages.
func lower(s string) string { return strings.ToLower(s) }

// foldElongation collapses runs of the same rune longer than max to max
// ("sooooo" → "soo" for max=2). It reports whether the token was elongated
// (had a run of 3 or more), which callers use to decide whether a further
// fold to single letters is a safe lookup fallback: folding "good" to "god"
// would be wrong, folding "goooood" to "god" is never attempted because the
// max=2 form "good" already matches.
func foldElongation(s string, max int) (string, bool) {
	elongated := false
	var prev rune
	run := 0
	needs := false
	for _, r := range s {
		if r == prev {
			run++
		} else {
			prev, run = r, 1
		}
		if run >= 3 {
			elongated = true
		}
		if run > max {
			needs = true
		}
	}
	if !needs {
		return s, elongated
	}
	var b strings.Builder
	b.Grow(len(s))
	prev, run = 0, 0
	for _, r := range s {
		if r == prev {
			run++
		} else {
			prev, run = r, 1
		}
		if run <= max {
			b.WriteRune(r)
		}
	}
	return b.String(), elongated
}

// isEmojiCluster reports whether a grapheme cluster is an emoji per §9.1: it
// contains an Extended_Pictographic code point, a combining keycap, or a pair
// of regional indicators (a flag). A lone regional indicator is not an emoji.
func isEmojiCluster(c string) bool {
	ri := 0
	for _, r := range c {
		switch {
		case r == combiningKeycap:
			return true
		case r >= 0x1F1E6 && r <= 0x1F1FF:
			ri++
		case isPictographic(r):
			return true
		}
	}
	return ri >= 2
}

// bmpPictographic lists the Extended_Pictographic ranges below U+1F000
// (Unicode emoji-data.txt). Ranges above U+1F000 are handled as one block.
var bmpPictographic = [][2]rune{
	{0x00A9, 0x00A9}, {0x00AE, 0x00AE}, {0x203C, 0x203C}, {0x2049, 0x2049},
	{0x2122, 0x2122}, {0x2139, 0x2139}, {0x2194, 0x2199}, {0x21A9, 0x21AA},
	{0x231A, 0x231B}, {0x2328, 0x2328}, {0x2388, 0x2388}, {0x23CF, 0x23CF},
	{0x23E9, 0x23F3}, {0x23F8, 0x23FA}, {0x24C2, 0x24C2}, {0x25AA, 0x25AB},
	{0x25B6, 0x25B6}, {0x25C0, 0x25C0}, {0x25FB, 0x25FE}, {0x2600, 0x2605},
	{0x2607, 0x2612}, {0x2614, 0x2685}, {0x2690, 0x2705}, {0x2708, 0x2712},
	{0x2714, 0x2714}, {0x2716, 0x2716}, {0x271D, 0x271D}, {0x2721, 0x2721},
	{0x2728, 0x2728}, {0x2733, 0x2734}, {0x2744, 0x2744}, {0x2747, 0x2747},
	{0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757},
	{0x2763, 0x2767}, {0x2795, 0x2797}, {0x27A1, 0x27A1}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2934, 0x2935}, {0x2B05, 0x2B07}, {0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x3030, 0x3030}, {0x303D, 0x303D},
	{0x3297, 0x3297}, {0x3299, 0x3299},
}

func isPictographic(r rune) bool {
	if r >= 0x1F000 {
		// Everything from U+1F000 to U+1FFFD is pictographic except regional
		// indicators and the skin-tone modifiers (Emoji_Component).
		return r <= 0x1FFFD && !(r >= 0x1F1E6 && r <= 0x1F1FF) && !isSkinTone(r)
	}
	if r < 0x00A9 {
		return false
	}
	for _, rg := range bmpPictographic {
		if r < rg[0] {
			return false
		}
		if r <= rg[1] {
			return true
		}
	}
	return false
}

func isSkinTone(r rune) bool { return r >= 0x1F3FB && r <= 0x1F3FF }

// emojiKey is the identity of an emoji: variation selectors removed, so ❤ and
// ❤\uFE0F are the same emoji. Skin tones are kept (the query layer folds them when
// emoji_normalise_modifiers is on).
func emojiKey(c string) string {
	if !strings.ContainsRune(c, variationSelector16) {
		return c
	}
	return strings.ReplaceAll(c, string(variationSelector16), "")
}

// baseEmoji strips skin-tone modifiers, used for lexicon lookups so 👍🏽 has
// the valence of 👍.
func baseEmoji(key string) string {
	if !strings.ContainsFunc(key, isSkinTone) {
		return key
	}
	var b strings.Builder
	for _, r := range key {
		if !isSkinTone(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// graphemes walks the grapheme clusters of s, returning the number of
// non-whitespace clusters (M-003 characters) and the emoji keys in order.
func graphemes(s string, emoji []string) (chars int, out []string) {
	out = emoji
	state := -1
	var c string
	for len(s) > 0 {
		c, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
		if isSpaceCluster(c) {
			continue
		}
		chars++
		if r, _ := utf8.DecodeRuneInString(c); r < 0x00A9 && len(c) == 1 {
			continue // fast path: plain ASCII is never an emoji
		}
		if isEmojiCluster(c) {
			out = append(out, emojiKey(c))
		}
	}
	return chars, out
}

func isSpaceCluster(c string) bool {
	for _, r := range c {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
