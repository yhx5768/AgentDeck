// Package knowledge is AgentDeck's curated knowledge store: SQLite entries
// with provenance, review state, and an FTS5 index.
//
// The store is deliberately independent of the board DB. It reads no files
// except its own database; artifact bytes stay under data/artifacts and are
// referenced by id.
package knowledge

import (
	"strings"
	"unicode"
)

// NormalizeFTS turns text into the searchable form indexed by FTS5.
//
// unicode61 treats a run of Han characters as one token, so a two-character
// query in the middle of a run ("编号" in "重编号") never matches. We therefore
// emit every Han character plus every adjacent bigram, then let MatchQuery
// build exact tokens / phrases from the same scheme.
func NormalizeFTS(text string) string {
	var out []string
	var han []rune
	var word []rune

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		out = append(out, strings.ToLower(string(word)))
		word = word[:0]
	}
	flushHan := func() {
		if len(han) == 0 {
			return
		}
		for _, r := range han {
			out = append(out, string(r))
		}
		for i := 0; i+1 < len(han); i++ {
			out = append(out, string(han[i:i+2]))
		}
		han = han[:0]
	}

	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return strings.Join(out, " ")
}

// MatchQuery builds an FTS5 MATCH expression from a user query using the same
// Han unigram/bigram scheme as NormalizeFTS.
//
//	看        -> "看"
//	看板      -> "看板"
//	重编号    -> "重编 编号"        (phrase = adjacent bigrams)
//	SSE 重编号 -> "sse" AND "重编 编号"
func MatchQuery(query string) string {
	var parts []string
	var han []rune
	var word []rune

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		parts = append(parts, quoteToken(strings.ToLower(string(word))))
		word = word[:0]
	}
	flushHan := func() {
		if len(han) == 0 {
			return
		}
		switch len(han) {
		case 1:
			parts = append(parts, quoteToken(string(han)))
		case 2:
			parts = append(parts, quoteToken(string(han)))
		default:
			bigrams := make([]string, 0, len(han)-1)
			for i := 0; i+1 < len(han); i++ {
				bigrams = append(bigrams, string(han[i:i+2]))
			}
			parts = append(parts, `"`+strings.Join(bigrams, " ")+`"`)
		}
		han = han[:0]
	}

	for _, r := range query {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return strings.Join(parts, " AND ")
}

func quoteToken(token string) string {
	return `"` + strings.ReplaceAll(token, `"`, "") + `"`
}

// QueryTerms returns the original terms used to locate a snippet in display
// text. Longer Han runs come first so a phrase match wins over a bigram.
func QueryTerms(query string) []string {
	var terms []string
	var han []rune
	var word []rune
	flushWord := func() {
		if len(word) > 0 {
			terms = append(terms, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushHan := func() {
		if len(han) == 0 {
			return
		}
		terms = append(terms, string(han))
		if len(han) > 2 {
			for i := 0; i+1 < len(han); i++ {
				terms = append(terms, string(han[i:i+2]))
			}
		}
		han = han[:0]
	}
	for _, r := range query {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return terms
}
