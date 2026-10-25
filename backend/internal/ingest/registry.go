package ingest

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrUnrecognised is returned by Detect when no parser is confident enough.
var ErrUnrecognised = errors.New("unrecognised export format")

var (
	registryMu sync.RWMutex
	registry   []Parser
)

// Register adds a parser. Parser packages call it from init so importing a
// parser package (usually blank-imported by the importer) enables it
// (FR-ING-021). Registering the same platform twice panics: it is a wiring bug.
func Register(p Parser) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, existing := range registry {
		if existing.Platform() == p.Platform() {
			panic(fmt.Sprintf("ingest: parser for %s registered twice", p.Platform()))
		}
	}
	registry = append(registry, p)
}

// Parsers returns the registered parsers sorted by platform name.
func Parsers() []Parser {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := append([]Parser(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Platform() < out[j].Platform() })
	return out
}

// Detect runs every registered parser's Detect on sample and returns the most
// confident one. If none reaches MinConfidence it returns ErrUnrecognised
// together with the best (low-confidence) detection, whose Hints explain why.
func Detect(sample []byte) (Parser, Detection, error) {
	if len(sample) > SampleSize {
		sample = sample[:SampleSize]
	}
	var (
		best    Parser
		bestDet Detection
	)
	for _, p := range Parsers() {
		d := p.Detect(sample)
		if best == nil || d.Confidence > bestDet.Confidence {
			best, bestDet = p, d
		}
	}
	if best == nil || bestDet.Confidence < MinConfidence {
		return nil, bestDet, ErrUnrecognised
	}
	return best, bestDet, nil
}
