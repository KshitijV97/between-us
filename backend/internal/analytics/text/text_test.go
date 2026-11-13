package text

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newAnalyzer builds the default English + Hinglish analyzer.
func newAnalyzer(t testing.TB) *Analyzer {
	t.Helper()
	a, err := New(Config{Languages: []string{"en", "hi-Latn"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWordsAndChars(t *testing.T) {
	a := newAnalyzer(t)
	tests := []struct {
		body         string
		words, chars int
	}{
		{"Good morning! ☀\uFE0F", 2, 13},
		{"don't stop", 2, 9},
		{"see https://example.com/a?b=1 now", 3, 31},
		{"👩\u200D❤\uFE0F\u200D👨", 0, 1}, // ZWJ sequence: one character, no words
		{"🇮🇳 India", 1, 6},              // flag is one character
		{"1\uFE0F\u20E3 first", 1, 6},   // keycap is an emoji, not a word
		{"我爱你", 3, 3},                   // CJK: one segment per ideograph (approximate)
		{"price 3.5 dollars", 3, 15},    // decimal stays one word
		{"   ", 0, 0},
		{"नमस्ते दोस्त", 2, 7}, // Devanagari: conjuncts are single graphemes
	}
	for _, tt := range tests {
		f := a.Analyze(tt.body)
		if f.WordCount != tt.words || f.CharCount != tt.chars {
			t.Errorf("%q: words=%d chars=%d, want %d/%d", tt.body, f.WordCount, f.CharCount, tt.words, tt.chars)
		}
	}
}

func TestEmojiIdentity(t *testing.T) {
	a := newAnalyzer(t)
	f := a.Analyze("❤\uFE0F ❤ 👍🏽 🇮🇳 1\uFE0F\u20E3 👩\u200D❤\uFE0F\u200D👨 © text")
	want := []string{"❤", "❤", "👍🏽", "🇮🇳", "1\u20E3", "👩\u200D❤\u200D👨", "©"}
	if !reflect.DeepEqual(f.Emoji, want) {
		t.Errorf("emoji = %q, want %q", f.Emoji, want)
	}
	if got := a.Analyze("no emoji here, just text :)").Emoji; len(got) != 0 {
		t.Errorf("emoticon counted without IncludeEmoticons: %q", got)
	}
	b, err := New(Config{Languages: []string{"en"}, IncludeEmoticons: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Analyze("nice :) <3").Emoji; !reflect.DeepEqual(got, []string{":)", "<3"}) {
		t.Errorf("emoticons = %q", got)
	}
}

func TestQuestions(t *testing.T) {
	a := newAnalyzer(t)
	for body, want := range map[string]bool{
		"Lunch?":                    true,
		"how are you":               true,
		"Do you want tea":           true,
		"kya kar rahe ho":           true,
		"kyu nahi aaye":             true, // variant kyu → kyun
		"kaise ho":                  true,
		"full width？":               true,
		"I think so":                false,
		"see https://x.com/?q=1":    false, // "?" inside a URL does not count
		"the interview I mean":      false,
		"whatever you say":          false,
		"dinner at 8 with everyone": false,
	} {
		if got := a.Analyze(body).IsQuestion; got != want {
			t.Errorf("%q: question=%v, want %v", body, got, want)
		}
	}
}

func TestLinks(t *testing.T) {
	a := newAnalyzer(t)
	f := a.Analyze("watch https://youtu.be/abc and https://www.youtube.com/watch?v=1, also www.Example.co.uk/page. and http://localhost:8080/x")
	want := []string{"youtu.be", "youtube.com", "example.co.uk", "localhost"}
	if !reflect.DeepEqual(f.LinkDomains, want) {
		t.Errorf("domains = %q, want %q", f.LinkDomains, want)
	}
	for _, term := range f.Terms {
		if strings.Contains(term, "youtube") || term == "www" || term == "com" {
			t.Errorf("URL fragment %q leaked into terms", term)
		}
	}
}

func TestSentiment(t *testing.T) {
	a := newAnalyzer(t)
	type want int // -1 negative, 0 neutral/unscored, 1 positive
	tests := []struct {
		body string
		sign want
	}{
		{"this is good", 1},
		{"this is not good", -1},
		{"accha nahi hai", -1},
		{"nahi accha", -1},
		{"nahi yaar, bahut accha hai", 1}, // "nahi" is not adjacent to "accha"
		{"bahut mast", 1},
		{"bekar movie", -1},
		{"I hate this", -1},
		{"ok", 0},
		{"😂", 1},
		{"😡", -1},
		{"goooood", 1}, // elongation folding
		{"it was bad but the ending was great", 1},
	}
	for _, tt := range tests {
		f := a.Analyze(tt.body)
		got := want(0)
		switch {
		case !f.SentimentScored:
		case f.Sentiment >= 0.05:
			got = 1
		case f.Sentiment <= -0.05:
			got = -1
		}
		if got != tt.sign {
			t.Errorf("%q: sentiment %.3f (scored=%v), want sign %d", tt.body, f.Sentiment, f.SentimentScored, tt.sign)
		}
		if f.Sentiment < -1 || f.Sentiment > 1 {
			t.Errorf("%q: sentiment %f out of range", tt.body, f.Sentiment)
		}
	}
	if a.Analyze("ok").SentimentScored {
		t.Error(`"ok" must not be scored (no lexicon hit)`)
	}
}

func TestSentimentModifiers(t *testing.T) {
	a := newAnalyzer(t)
	s := func(b string) float64 { return a.Analyze(b).Sentiment }
	if !(s("very good") > s("good")) {
		t.Error("intensifier should raise the score")
	}
	if !(s("bahut accha") > s("accha")) {
		t.Error("Hinglish intensifier should raise the score")
	}
	if !(s("slightly good") < s("good")) {
		t.Error("dampener should lower the score")
	}
	if !(s("this is GOOD") > s("this is good")) {
		t.Error("ALL-CAPS word in a mixed-case message should raise the score")
	}
	if !(s("good!!!") > s("good")) {
		t.Error("exclamations should raise the score")
	}
	if !(s("don’t like it") < 0) {
		t.Error("curly apostrophe negation should work")
	}
}

func TestAffection(t *testing.T) {
	a := newAnalyzer(t)
	for body, want := range map[string]int{
		"miss you":           1,
		"miss you ❤\uFE0F":   2,
		"I love you":         1, // longest match, no double count
		"don't miss you":     0, // negated
		"yaad aa rahi hai":   1,
		"miss kar raha hu":   1,
		"jaan":               1,
		"jaana hai kal":      0, // whole_word: "jaana" (to go) is not "jaan"
		"good night 😘🥰":      2,
		"see you tomorrow":   0,
		"apna khayal rakhna": 1,
	} {
		if got := a.Analyze(body).AffectionHits; got != want {
			t.Errorf("%q: affection=%d, want %d", body, got, want)
		}
	}
}

func TestConflict(t *testing.T) {
	a := newAnalyzer(t)
	for body, want := range map[string]bool{
		"leave me alone":                     true,  // strong phrase
		"chup kar":                           true,  // strong (Hinglish)
		"whatever":                           false, // weak alone
		"whatever, you are so stupid":        true,  // phrase + strongly negative
		"WHY WOULD YOU DO THAT":              false, // shouting alone
		"I HATE THIS SO MUCH!!!":             true,  // shouting + negative + punctuation
		"you never listen, this is terrible": true,
		"WHATEVER 😂":                         true,  // per spec: weak phrase + shouting are two signals
		"whatever 😂":                         false, // playful: weak phrase alone
		"kya!!!":                             false,
	} {
		if got := a.Analyze(body).Conflict; got != want {
			f := a.Analyze(body)
			t.Errorf("%q: conflict=%v, want %v (sentiment %.2f)", body, got, want, f.Sentiment)
		}
	}
}

func TestTermsAndBigrams(t *testing.T) {
	a := newAnalyzer(t)
	f := a.Analyze("Acha, the movie was sooooo achha! Movie night 2nite at 8 with Rahul")
	wantTerms := []string{"movie", "movie", "night", "2nite", "rahul"}
	if !reflect.DeepEqual(f.Terms, wantTerms) {
		t.Errorf("terms = %q, want %q", f.Terms, wantTerms)
	}
	if !reflect.DeepEqual(f.Bigrams, []string{"movie night", "night 2nite"}) {
		t.Errorf("bigrams = %q", f.Bigrams)
	}
	// Variant folding: all spellings become one term.
	g := a.Analyze("pyar pyaar")
	if !reflect.DeepEqual(g.Terms, []string{"pyaar", "pyaar"}) {
		t.Errorf("variant folding: %q", g.Terms)
	}
}

// The reference example of docs/04-metrics-catalog.md §7 (text messages only;
// message 5 is media-only).
func TestReferenceExample(t *testing.T) {
	a := newAnalyzer(t)
	A := []string{"Good morning! ☀\uFE0F", "It went well!! I think 🤞", "Yes! 20 min", "miss you ❤\uFE0F", "😴 good night"}
	R := []string{"Morning 🙂 how did it go?", "the interview I mean", "Lunch?", "miss you too ❤\uFE0F"}
	sum := func(msgs []string) (words, emoji, questions, affectionMsgs int) {
		for _, m := range msgs {
			f := a.Analyze(m)
			words += f.WordCount
			emoji += len(f.Emoji)
			if f.IsQuestion {
				questions++
			}
			if f.AffectionHits > 0 {
				affectionMsgs++
			}
		}
		return
	}
	if w, e, q, am := sum(A); w != 14 || e != 4 || q != 0 || am != 1 {
		t.Errorf("A: words=%d emoji=%d questions=%d affection=%d, want 14/4/0/1", w, e, q, am)
	}
	if w, e, q, am := sum(R); w != 13 || e != 2 || q != 2 || am != 1 {
		t.Errorf("R: words=%d emoji=%d questions=%d affection=%d, want 13/2/2/1", w, e, q, am)
	}
}

func TestLexiconOverrideAndHash(t *testing.T) {
	base := newAnalyzer(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "en"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "en", "sentiment.tsv"), []byte("# version: 2\nzorbly\t3.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	over, err := New(Config{Languages: []string{"en", "hi-Latn"}, LexiconDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !over.Analyze("zorbly").SentimentScored || base.Analyze("zorbly").SentimentScored {
		t.Error("override sentiment file not used")
	}
	if over.LexiconHash() == base.LexiconHash() || base.LexiconHash() != newAnalyzer(t).LexiconHash() {
		t.Error("hash must be stable and change with lexicon content")
	}
	if _, err := New(Config{Languages: []string{"xx-Nope"}}); err == nil {
		t.Error("unknown language must be an error")
	}
}

func TestConcurrentUse(t *testing.T) {
	a := newAnalyzer(t)
	done := make(chan bool)
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				a.Analyze("bahut accha!! miss you ❤\uFE0F https://youtu.be/x kya scene hai?")
			}
			done <- true
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

var benchBodies = []string{
	"good morning ☀\uFE0F", "kya kar rahe ho?", "haha 😂", "ok", "on my way",
	"miss you ❤\uFE0F", "call me when free", "kal milte hain", "that meeting was so long 😩",
	"did you eat?", "yes, dal chawal 😋", "running late, sorry!", "love you", "kitna kaam hai yaar",
	"see you at 7", "👍", "the movie was amazing https://youtu.be/abc", "where are you?",
	"traffic bahut hai 🚗", "happy birthday!! 🎉🎂",
}

// BenchmarkAnalyze reports the cost per typical message; multiply ns/op by
// 100k for the NFR-PERF-002 budget (< 3 s for features).
func BenchmarkAnalyze(b *testing.B) {
	a := newAnalyzer(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		a.Analyze(benchBodies[i%len(benchBodies)])
	}
}

func FuzzAnalyze(f *testing.F) {
	for _, s := range benchBodies {
		f.Add(s)
	}
	f.Add("👩\u200D❤\uFE0F\u200D👨 1\uFE0F\u20E3 🇮🇳 www.")
	f.Add("NOT not nahi!!!???")
	a := newAnalyzer(f)
	f.Fuzz(func(t *testing.T, s string) {
		x := a.Analyze(s)
		if x.WordCount < 0 || x.CharCount < 0 || x.AffectionHits < 0 {
			t.Fatalf("negative count for %q: %+v", s, x)
		}
		if x.Sentiment < -1 || x.Sentiment > 1 || x.Sentiment != x.Sentiment {
			t.Fatalf("sentiment out of range for %q: %v", s, x.Sentiment)
		}
		if len(x.Emoji) > x.CharCount+len(strings.Fields(s)) {
			t.Fatalf("more emoji than characters for %q", s)
		}
	})
}
