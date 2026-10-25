package whatsapp

import (
	"regexp"
	"strconv"
	"strings"
)

// Header regexes (docs/parsers/whatsapp.md §3). They run on a pre-normalised
// line (see normalizeLine), are RE2 (linear time on hostile input, SEC-003) and
// deliberately do not decide the date order: the three numeric date fields are
// captured raw and interpreted once per file by date-order inference (§4).
const (
	datePart = `(\d{1,4})[./-](\d{1,2})[./-](\d{1,4})`
	timePart = `(\d{1,2})[:.](\d{2})(?:[:.](\d{2}))?`
	// Meridiem: AM, am, a.m., a. m. (localised forms are rewritten to AM/PM
	// by normalizeLine before matching).
	merPart = `(?:\s?((?i:[ap]\.?\s?m\.?)))?`
)

var (
	// androidRe matches "<date>, <time>[ mer] - <rest>" (variants A1–A6; the
	// comma is optional for A5).
	androidRe = regexp.MustCompile(`^` + datePart + `,?\s` + timePart + merPart + `\s-\s(.*)$`)
	// iosRe matches "[<date>, <time>[ mer]] <rest>" (variants I1–I3).
	iosRe = regexp.MustCompile(`^\[` + datePart + `,?\s` + timePart + merPart + `\]\s?(.*)$`)

	// localMeridiem rewrites non-Latin and long-form meridiem tokens that
	// directly follow the header time into AM/PM, so one regex covers them.
	// Only the token right after the time is touched; bodies are never edited.
	localMeridiem = regexp.MustCompile(
		`^(\[?\d{1,4}[./-]\d{1,2}[./-]\d{1,4},?\s\d{1,2}[:.]\d{2}(?:[:.]\d{2})?)\s?` +
			`(μ\.μ\.|π\.μ\.|午後|午前|nachm\.|vorm\.|du\.|de\.)`)
	localMeridiemMap = map[string]string{
		"μ.μ.": "PM", "π.μ.": "AM", "午後": "PM", "午前": "AM",
		"nachm.": "PM", "vorm.": "AM", "du.": "PM", "de.": "AM",
	}

	// headerMarks are invisible directional and formatting characters that
	// exports scatter through header lines (§2.1 step 3): U+200E, U+200F,
	// U+202A–U+202E, U+2066–U+2069 and U+FEFF.
	headerMarks = strings.NewReplacer(
		"\u200e", "", "\u200f", "",
		"\u202a", "", "\u202b", "", "\u202c", "", "\u202d", "", "\u202e", "",
		"\u2066", "", "\u2067", "", "\u2068", "", "\u2069", "",
		"\ufeff", "",
		// §2.1 step 4: NBSP and NARROW NBSP (iOS 17, Android 2023+) → space.
		"\u00a0", " ", "\u202f", " ",
	)
)

// header is one parsed header line. Date fields stay raw until the file's
// date order is known.
type header struct {
	f      [3]int // numeric date fields in written order
	digits [3]int // digit counts, needed for the YMD rule and year expansion
	hour   int
	min    int
	sec    int
	hasSec bool
	// mer is 0 (24-hour clock), 'A' or 'P'.
	mer byte
	// merLower records a lower-case meridiem ("pm"), a locale hint (§4 step 6).
	merLower bool
	ios      bool
	// rest is everything after the header: "Sender: text" or a system text.
	rest string
}

// normalizeLine prepares a raw line for header matching (§2.1): trailing CR
// removed, invisible marks stripped, special spaces turned into ASCII spaces
// and localised meridiems rewritten. The result is used for header lines only;
// continuation lines are stored from the raw text.
func normalizeLine(raw string) string {
	s := strings.TrimRight(raw, "\r")
	s = headerMarks.Replace(s)
	if m := localMeridiem.FindStringSubmatchIndex(s); m != nil {
		tok := s[m[4]:m[5]]
		s = s[:m[3]] + " " + localMeridiemMap[tok] + s[m[5]:]
	}
	return s
}

// parseHeader recognises an Android or iOS header line. It validates only the
// shape; calendar validity is checked later under the decided date order.
func parseHeader(norm string) (header, bool) {
	// Cheap pre-check: headers start with a digit or '['. Most continuation
	// lines are rejected here without running a regex.
	if norm == "" || (norm[0] != '[' && (norm[0] < '0' || norm[0] > '9')) {
		return header{}, false
	}
	var (
		m   []string
		ios bool
	)
	if norm[0] == '[' {
		m, ios = iosRe.FindStringSubmatch(norm), true
	} else {
		m = androidRe.FindStringSubmatch(norm)
	}
	if m == nil {
		return header{}, false
	}
	h := header{ios: ios, rest: m[8]}
	for i := 0; i < 3; i++ {
		h.f[i], _ = strconv.Atoi(m[1+i]) // digits only, at most 4: cannot fail
		h.digits[i] = len(m[1+i])
	}
	h.hour, _ = strconv.Atoi(m[4])
	h.min, _ = strconv.Atoi(m[5])
	if m[6] != "" {
		h.sec, _ = strconv.Atoi(m[6])
		h.hasSec = true
	}
	if mer := m[7]; mer != "" {
		switch mer[0] {
		case 'a', 'A':
			h.mer = 'A'
		default:
			h.mer = 'P'
		}
		h.merLower = mer[0] == 'a' || mer[0] == 'p'
	}
	return h, true
}

// clock returns the 24-hour time of day, or ok=false when the fields are out
// of range for the clock style (e.g. "13:05 PM" or "24:10").
func (h header) clock() (hour, min, sec int, ok bool) {
	hour = h.hour
	switch h.mer {
	case 0:
		if hour > 23 {
			return 0, 0, 0, false
		}
	case 'A', 'P':
		if hour < 1 || hour > 12 {
			return 0, 0, 0, false
		}
		hour %= 12 // 12 AM → 0, 12 PM → 12 below
		if h.mer == 'P' {
			hour += 12
		}
	}
	if h.min > 59 || h.sec > 59 {
		return 0, 0, 0, false
	}
	return hour, h.min, h.sec, true
}
