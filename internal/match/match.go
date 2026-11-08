// Package match finds which intent a piece of text means, using plain word overlap.
// It is deliberately strict: a request it is unsure about is a miss, never a guess.
package match

import (
	"sort"
	"strings"
	"unicode"
)

// filler words carry no meaning for picking an intent. Words that change meaning,
// such as "is", "not", "dont" or "what", are kept, so questions and negations do
// not match commands.
var filler = map[string]bool{
	"please": true, "the": true, "a": true, "an": true, "can": true, "could": true,
	"would": true, "will": true, "you": true, "u": true, "hey": true, "ok": true,
	"okay": true, "just": true, "now": true, "kindly": true, "thanks": true,
	"thank": true, "its": true, "go": true, "ahead": true, "and": true,
}

var numbers = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
	"eleven": "11", "twelve": "12", "fifteen": "15", "twenty": "20", "thirty": "30",
	"forty": "40", "fifty": "50", "hundred": "100",
}

// Words returns the meaningful words of text: lowercased, punctuation removed
// (apostrophes join, so "it's" is "its"), filler dropped, number words as digits.
// Order is kept and duplicates are removed.
func Words(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '’':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(b.String()) {
		if filler[w] {
			continue
		}
		if d, ok := numbers[w]; ok {
			w = d
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// Normalize is Words joined with single spaces.
func Normalize(text string) string { return strings.Join(Words(text), " ") }

// Phrase is one way of saying an intent.
type Phrase struct {
	Key  string // the entity the phrase belongs to
	Text string
}

// Candidate is a phrase and how well the text matched it, from 0 to 1.
type Candidate struct {
	Key    string  `json:"key"`
	Phrase string  `json:"phrase"`
	Score  float64 `json:"score"`
}

// Options are the two gates a match must pass.
type Options struct {
	MinScore  float64 // the best phrase must score at least this
	MinMargin float64 // and beat the best phrase of any other entity by this much
}

// Result is the outcome of matching one text.
type Result struct {
	Normalized string      `json:"normalized"`
	Candidates []Candidate `json:"candidates"` // best per entity, highest first, at most 3
	Key        string      `json:"key,omitempty"`
	Matched    bool        `json:"matched"`
}

// Score is the word-set overlap (Jaccard) of two word lists.
func Score(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, w := range a {
		set[w] = true
	}
	both := 0
	for _, w := range b {
		if set[w] {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// Find matches text against phrases.
func Find(text string, phrases []Phrase, opt Options) Result {
	words := Words(text)
	res := Result{Normalized: strings.Join(words, " ")}
	best := map[string]Candidate{}
	for _, p := range phrases {
		s := Score(words, Words(p.Text))
		if c, ok := best[p.Key]; !ok || s > c.Score {
			best[p.Key] = Candidate{Key: p.Key, Phrase: p.Text, Score: s}
		}
	}
	for _, c := range best {
		if c.Score > 0 {
			res.Candidates = append(res.Candidates, c)
		}
	}
	sort.Slice(res.Candidates, func(i, j int) bool {
		a, b := res.Candidates[i], res.Candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Key < b.Key
	})
	if len(res.Candidates) > 3 {
		res.Candidates = res.Candidates[:3]
	}
	if len(res.Candidates) == 0 {
		return res
	}
	top := res.Candidates[0]
	runnerUp := 0.0
	if len(res.Candidates) > 1 {
		runnerUp = res.Candidates[1].Score
	}
	if top.Score >= opt.MinScore && top.Score-runnerUp >= opt.MinMargin {
		res.Key, res.Matched = top.Key, true
	}
	return res
}
