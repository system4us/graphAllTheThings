package graph

import (
	"strings"
	"unicode"
)

// queryStopwords are question words that carry no search signal in English
// or Spanish ("how does X get verified", "dónde se bloquea X"). Matching on
// them ranks every node that happens to say "get" or "the" — including a
// const literally named get.
var queryStopwords = map[string]bool{
	// English
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "at": true, "by": true, "for": true, "from": true, "with": true,
	"into": true, "about": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "being": true, "do": true, "does": true, "did": true, "done": true,
	"how": true, "what": true, "where": true, "when": true, "which": true, "who": true,
	"why": true, "get": true, "gets": true, "got": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true, "there": true, "i": true,
	"we": true, "you": true, "can": true, "could": true, "should": true, "would": true,
	"will": true, "if": true, "then": true, "than": true, "so": true, "any": true,
	"some": true, "all": true, "my": true, "our": true, "me": true, "us": true,
	"handled": true, "happens": true, "work": true, "works": true,
	// Spanish
	"como": true, "cómo": true, "donde": true, "dónde": true, "que": true, "qué": true,
	"se": true, "el": true, "la": true, "los": true, "las": true, "un": true, "una": true,
	"unos": true, "unas": true, "de": true, "del": true, "en": true, "por": true,
	"para": true, "con": true, "es": true, "son": true, "y": true, "o": true, "al": true,
	"lo": true, "le": true, "su": true, "sus": true, "cuando": true, "cuándo": true,
	"cual": true, "cuál": true, "esta": true, "este": true, "hace": true, "funciona": true,
}

// QueryTerms splits a natural-language query into lower-cased search terms:
// split on anything that is not a letter or digit, stopwords dropped. When
// every word is a stopword (a query like "get"), the words are kept: the
// user meant them.
func QueryTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	var out []string
	for _, f := range fields {
		if !queryStopwords[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return fields
	}
	return out
}

// stemSuffixes are stripped (longest first) so a prefix query finds the other
// forms of a word: verified/verifies/verification → verif*, masking → mask*.
var stemSuffixes = []string{
	"ications", "ication", "ations", "ation", "ements", "ement", "ings", "ing",
	"ied", "ies", "ers", "er", "ed", "es", "s", "y", "e",
}

// Stem returns a light English stem of a lower-cased term, never shorter than
// four characters (short words are kept whole: a 3-letter prefix matches
// everything).
func Stem(t string) string {
	for _, suf := range stemSuffixes {
		if strings.HasSuffix(t, suf) && len([]rune(t))-len([]rune(suf)) >= 4 {
			return strings.TrimSuffix(t, suf)
		}
	}
	return t
}
