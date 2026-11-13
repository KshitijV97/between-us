package text

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// embedded holds the default lexicons (docs/04-metrics-catalog.md §9.3). Plain
// TSV/TXT files keep the binary free of a YAML dependency and are easy to
// review in pull requests.
//
//go:embed lexicons
var embedded embed.FS

// Files loaded for every configured language. All are optional except that a
// language must have at least one of them (otherwise it is a typo).
var languageFiles = []string{
	"stopwords.txt", "interrogatives.txt", "sentiment.tsv", "negations.txt",
	"intensifiers.tsv", "affection.tsv", "conflict.tsv", "variants.tsv",
}

// phrase is a multi-word lexicon entry matched on normalised tokens.
type phrase struct {
	tokens    []string
	weight    float64
	wholeWord bool // single-token phrases without it also match as a prefix
	strong    bool // conflict phrases only
}

// negation describes a negating token.
type negation struct {
	// both means the word also negates up to two following tokens, as in
	// Hindi "accha nahi hai"; otherwise it negates up to three preceding
	// tokens' successors (English "not good").
	both bool
}

// lexicon is the merged, read-only lexicon set of the configured languages.
type lexicon struct {
	stop           map[string]struct{}
	interrogatives [][]string
	sentiment      map[string]float64
	negations      map[string]negation
	intensifiers   map[string]float64
	affection      map[string][]phrase // keyed by first token, longest first
	affectionPfx   []phrase            // single-token prefix phrases
	conflict       map[string][]phrase // keyed by first token, longest first
	variants       map[string]string
	emojiSentiment map[string]float64
	emojiAffection map[string]struct{}
	hash           string
}

// source reads lexicon files, preferring an override directory.
type source struct {
	override fs.FS // nil when no override directory
}

func (s source) read(name string) ([]byte, error) {
	if s.override != nil {
		b, err := fs.ReadFile(s.override, name)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return fs.ReadFile(embedded, path.Join("lexicons", name))
}

// loadLexicon reads and merges the lexicons of langs plus the emoji lexicons.
func loadLexicon(langs []string, dir string) (*lexicon, error) {
	src := source{}
	if dir != "" {
		src.override = os.DirFS(dir)
	}
	lx := &lexicon{
		stop: map[string]struct{}{}, sentiment: map[string]float64{},
		negations: map[string]negation{}, intensifiers: map[string]float64{},
		affection: map[string][]phrase{}, conflict: map[string][]phrase{},
		variants: map[string]string{}, emojiSentiment: map[string]float64{},
		emojiAffection: map[string]struct{}{},
	}
	files := map[string][]byte{} // name -> content, for the hash
	read := func(name string) (string, bool, error) {
		b, err := src.read(name)
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("lexicon %s: %w", name, err)
		}
		files[name] = b
		return string(b), true, nil
	}

	// Variants first: every other entry is folded through them so lexicon
	// authors can write any spelling.
	for _, lang := range langs {
		if body, ok, err := read(lang + "/variants.tsv"); err != nil {
			return nil, err
		} else if ok {
			for _, f := range records(body) {
				if len(f) >= 2 {
					lx.variants[lower(f[0])] = lower(f[1])
				}
			}
		}
	}

	for _, lang := range langs {
		found := false
		for _, name := range languageFiles {
			body, ok, err := read(lang + "/" + name)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			found = true
			if err := lx.add(name, body); err != nil {
				return nil, fmt.Errorf("lexicon %s/%s: %w", lang, name, err)
			}
		}
		if !found {
			return nil, fmt.Errorf("no lexicons for language %q", lang)
		}
	}

	if body, ok, err := read("emoji/sentiment.tsv"); err != nil {
		return nil, err
	} else if ok {
		for _, f := range records(body) {
			if len(f) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				return nil, fmt.Errorf("emoji/sentiment.tsv: %q: %w", f[0], err)
			}
			lx.emojiSentiment[emojiKey(f[0])] = v
		}
	}
	if body, ok, err := read("emoji/affection.txt"); err != nil {
		return nil, err
	} else if ok {
		for _, f := range records(body) {
			lx.emojiAffection[emojiKey(f[0])] = struct{}{}
		}
	}

	for _, m := range []map[string][]phrase{lx.affection, lx.conflict} {
		for k := range m {
			sort.SliceStable(m[k], func(i, j int) bool { return len(m[k][i].tokens) > len(m[k][j].tokens) })
		}
	}
	lx.hash = hashFiles(files)
	return lx, nil
}

// add parses one language file into the lexicon.
func (lx *lexicon) add(name, body string) error {
	for _, f := range records(body) {
		switch name {
		case "stopwords.txt":
			lx.stop[lx.fold(lower(f[0]))] = struct{}{}
		case "interrogatives.txt":
			if toks := lx.phraseTokens(f[0]); len(toks) > 0 {
				lx.interrogatives = append(lx.interrogatives, toks)
			}
		case "sentiment.tsv":
			if len(f) < 2 {
				return fmt.Errorf("%q: missing valence", f[0])
			}
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil || v < -4 || v > 4 {
				return fmt.Errorf("%q: valence must be a number in [-4, 4]", f[0])
			}
			lx.sentiment[lx.fold(lower(f[0]))] = v
		case "negations.txt":
			toks := lx.phraseTokens(f[0])
			if len(toks) != 1 {
				continue // multi-word negations are not supported; ignore them
			}
			lx.negations[toks[0]] = negation{both: len(f) > 1 && f[1] == "both"}
		case "intensifiers.tsv":
			if len(f) < 2 {
				return fmt.Errorf("%q: missing multiplier", f[0])
			}
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil || v <= 0 {
				return fmt.Errorf("%q: multiplier must be a positive number", f[0])
			}
			lx.intensifiers[lx.fold(lower(f[0]))] = v
		case "affection.tsv":
			p := phrase{tokens: lx.phraseTokens(f[0]), weight: 1}
			if len(f) > 1 {
				if w, err := strconv.ParseFloat(f[1], 64); err == nil {
					p.weight = w
				}
			}
			p.wholeWord = len(f) > 2 && f[2] == "whole_word"
			if len(p.tokens) == 0 {
				continue
			}
			if len(p.tokens) == 1 && !p.wholeWord {
				lx.affectionPfx = append(lx.affectionPfx, p)
				continue
			}
			lx.affection[p.tokens[0]] = append(lx.affection[p.tokens[0]], p)
		case "conflict.tsv":
			p := phrase{tokens: lx.phraseTokens(f[0]), strong: len(f) > 1 && f[1] == "strong", wholeWord: true}
			if len(p.tokens) == 0 {
				continue
			}
			lx.conflict[p.tokens[0]] = append(lx.conflict[p.tokens[0]], p)
		case "variants.tsv":
			// already loaded
		}
	}
	return nil
}

// phraseTokens normalises a lexicon phrase exactly like message text, so
// "Miss  you" in a file matches "miss you" in a chat.
func (lx *lexicon) phraseTokens(s string) []string {
	var out []string
	forEachWord(prepare(s), func(w string) {
		out = append(out, lx.fold(lower(w)))
	})
	return out
}

// fold applies elongation folding (runs of 3+ letters → 2) and variant
// folding, the canonical form used for every lookup.
func (lx *lexicon) fold(tok string) string {
	f, _ := foldElongation(tok, 2)
	if v, ok := lx.variants[f]; ok {
		return v
	}
	return f
}

// records yields the tab-separated fields of each non-empty, non-comment line.
func records(body string) [][]string {
	var out [][]string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if parts[0] == "" {
			continue
		}
		out = append(out, parts)
	}
	return out
}

// hashFiles is a stable digest of the loaded lexicon files (names and
// contents), part of the rollup-version bookkeeping (§11.1).
func hashFiles(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(files[n])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
