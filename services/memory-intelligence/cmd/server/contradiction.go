package main

import (
	"regexp"
	"strings"
)

// Contradiction detection compares what two facts state, not their words.
// A fact is reduced to its proposition -- its content words, lower-cased,
// with negations, auxiliaries and articles removed and simple inflections
// folded ("uses"/"use") -- and its polarity, negative when it carries an
// odd number of negations. The same scheme as the consolidation worker's
// statements.py, so the worker and this service agree on what contradicts.

var (
	propositionToken = regexp.MustCompile(`[a-z0-9][a-z0-9'._/:-]*[a-z0-9]|[a-z0-9]`)
	noLonger         = regexp.MustCompile(`\bno longer\b`)
	negationWords    = map[string]bool{
		"not": true, "no": true, "never": true, "none": true, "nobody": true, "nothing": true, "neither": true, "nor": true,
		"without": true, "cannot": true, "dont": true, "doesnt": true, "didnt": true, "isnt": true, "arent": true,
		"wasnt": true, "werent": true, "wont": true, "shouldnt": true, "mustnt": true, "hasnt": true, "havent": true,
		"hadnt": true, "couldnt": true, "wouldnt": true,
	}
	propositionStopwords = map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
		"being": true, "am": true, "do": true, "does": true, "did": true, "has": true, "have": true, "had": true,
		"will": true, "would": true, "shall": true, "should": true, "can": true, "could": true, "may": true, "might": true,
		"must": true, "to": true, "of": true, "and": true, "or": true, "that": true, "this": true, "these": true,
		"those": true, "it": true, "its": true, "we": true, "our": true, "us": true, "they": true, "their": true,
		"i": true, "my": true, "you": true, "your": true, "he": true, "she": true, "his": true, "her": true,
		"them": true, "on": true, "in": true, "at": true, "by": true, "for": true, "with": true, "as": true, "so": true,
		"any": true, "all": true, "also": true, "just": true, "still": true, "very": true, "really": true,
	}
)

// sameProposition is the share of the smaller proposition the larger one
// must contain for two statements to say the same thing.
const sameProposition = 0.85

type proposition struct {
	words    map[string]bool
	negative bool
}

func foldInflection(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 4 && (strings.HasSuffix(w, "ses") || strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "zes") || strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes")):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

func parseProposition(text string) proposition {
	p := proposition{words: map[string]bool{}}
	negations := 0
	lowered := noLonger.ReplaceAllString(strings.ToLower(text), "not")
	for _, raw := range propositionToken.FindAllString(lowered, -1) {
		tok := strings.Trim(raw, "'")
		if negationWords[strings.ReplaceAll(tok, "'", "")] || strings.HasSuffix(tok, "n't") {
			negations++
			continue
		}
		if propositionStopwords[tok] || len(tok) < 2 {
			continue
		}
		p.words[foldInflection(tok)] = true
	}
	p.negative = negations%2 == 1
	return p
}

func propositionOverlap(a, b proposition) float64 {
	if len(a.words) == 0 || len(b.words) == 0 {
		return 0
	}
	shared := 0
	for w := range a.words {
		if b.words[w] {
			shared++
		}
	}
	smaller := len(a.words)
	if len(b.words) < smaller {
		smaller = len(b.words)
	}
	return float64(shared) / float64(smaller)
}

// isContradictory reports whether two facts in the same network and scope
// cannot both hold: they state the same proposition with opposite polarity
// ("we use PostgreSQL" / "we do not use PostgreSQL"), or they give the same
// key two different values ("deploys ship every tuesday" / "... every
// thursday"). A value that only adds detail to the other ("on friday" /
// "on friday afternoon") refines it and is not a contradiction.
func isContradictory(a, b *Fact) bool {
	if a.Network != b.Network || a.Scope != b.Scope {
		return false
	}
	pa, pb := parseProposition(a.Content), parseProposition(b.Content)
	if pa.negative != pb.negative && len(pa.words) >= 2 && len(pb.words) >= 2 && propositionOverlap(pa, pb) >= sameProposition {
		return true
	}
	if pa.negative != pb.negative {
		return false
	}
	aKey, aVal := keyValueNorm(a.Content)
	bKey, bVal := keyValueNorm(b.Content)
	if aKey == "" || aKey != bKey || aVal == "" || bVal == "" || aVal == bVal {
		return false
	}
	return !wordSubset(aVal, bVal) && !wordSubset(bVal, aVal)
}

// wordSubset reports whether every word of a is in b.
func wordSubset(a, b string) bool {
	have := map[string]bool{}
	for _, w := range strings.Fields(b) {
		have[w] = true
	}
	for _, w := range strings.Fields(a) {
		if !have[w] {
			return false
		}
	}
	return true
}
