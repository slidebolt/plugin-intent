package match

import (
	"reflect"
	"testing"
)

var opt = Options{MinScore: 0.8, MinMargin: 0.15}

var phrases = []Phrase{
	{"p.intents.porch_off", "turn off the porch"},
	{"p.intents.porch_off", "porch off"},
	{"p.intents.porch_on", "turn on the porch"},
	{"p.intents.porch_on", "porch on"},
	{"p.intents.reading", "it's reading time"},
	{"p.intents.dinner", "dinner time"},
}

func TestWords(t *testing.T) {
	cases := map[string][]string{
		"Please turn OFF the porch!":    {"turn", "off", "porch"},
		"It's reading time.":            {"reading", "time"},
		"set it to fifty":               {"set", "it", "to", "50"},
		"is the porch off?":             {"is", "porch", "off"},
		"don't turn off the porch":      {"dont", "turn", "off", "porch"},
		"porch, porch off":              {"porch", "off"},
		"":                              nil,
		"  could you   kindly, please ": nil,
	}
	for in, want := range cases {
		if got := Words(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Words(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScore(t *testing.T) {
	if s := Score([]string{"a", "b"}, []string{"b", "a"}); s != 1 {
		t.Fatalf("same set = %v", s)
	}
	if s := Score([]string{"turn", "off", "porch"}, []string{"turn", "on", "porch"}); s != 0.5 {
		t.Fatalf("opposites = %v", s)
	}
	if s := Score(nil, []string{"a"}); s != 0 {
		t.Fatalf("empty = %v", s)
	}
}

func TestFind(t *testing.T) {
	cases := []struct {
		text string
		want string // "" means no match
	}{
		{"turn off the porch", "p.intents.porch_off"},
		{"Please, turn the porch off.", "p.intents.porch_off"},
		{"porch off", "p.intents.porch_off"},
		{"turn on the porch", "p.intents.porch_on"},
		{"it's reading time", "p.intents.reading"},
		{"reading time please", "p.intents.reading"},
		// Opposites, questions and negations never press anything.
		{"is the porch off", ""},
		{"don't turn off the porch", ""},
		// Close but different wording is a miss, not a guess.
		{"turn off the porch light", ""},
		{"it's weeding time", ""},
		{"porch", ""},
		{"", ""},
	}
	for _, c := range cases {
		r := Find(c.text, phrases, opt)
		if r.Key != c.want || r.Matched != (c.want != "") {
			t.Errorf("Find(%q) = %q matched=%v, want %q; candidates %+v", c.text, r.Key, r.Matched, c.want, r.Candidates)
		}
	}
}

func TestFindRejectsNearTies(t *testing.T) {
	tie := []Phrase{{"x", "movie time"}, {"y", "time movie"}}
	if r := Find("movie time", tie, opt); r.Matched {
		t.Fatalf("a tie between two entities must not match: %+v", r)
	}
	// Two phrases of the same entity are not a tie.
	same := []Phrase{{"x", "movie time"}, {"x", "time movie"}}
	if r := Find("movie time", same, opt); !r.Matched || r.Key != "x" {
		t.Fatalf("phrases of one entity must not compete: %+v", r)
	}
}

func TestFindCandidates(t *testing.T) {
	r := Find("turn off the porch", phrases, opt)
	if r.Normalized != "turn off porch" {
		t.Fatalf("normalized = %q", r.Normalized)
	}
	if len(r.Candidates) != 2 || r.Candidates[0].Score != 1 || r.Candidates[1].Key != "p.intents.porch_on" {
		t.Fatalf("candidates = %+v", r.Candidates)
	}
}
