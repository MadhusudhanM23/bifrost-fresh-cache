package main

import (
	"reflect"
	"testing"
)

func TestContentWords(t *testing.T) {
	got := ContentWords("How do I export my Contacts to a CSV file?", nil)
	want := []string{"contact", "csv", "export", "file"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestContentWordsSynonyms(t *testing.T) {
	a := ContentWords("What does the Pro plan cost?", defaultSynonyms)
	b := ContentWords("What is the price of the Pro plan?", defaultSynonyms)
	x, y := Diff(a, b)
	if len(x) != 0 || len(y) != 0 {
		t.Fatalf("cost and price should be one word: %v %v", x, y)
	}
	a = ContentWords("What does the Pro plan cost?", nil)
	b = ContentWords("What is the price of the Pro plan?", nil)
	x, y = Diff(a, b)
	if len(x) == 0 || len(y) == 0 {
		t.Fatalf("without synonyms cost and price differ: %v %v", x, y)
	}
}

func TestDiff(t *testing.T) {
	x, y := Diff([]string{"csv", "export"}, []string{"csv", "import"})
	if !reflect.DeepEqual(x, []string{"export"}) || !reflect.DeepEqual(y, []string{"import"}) {
		t.Fatalf("got %v %v", x, y)
	}
	x, y = Diff([]string{"a"}, []string{"a"})
	if x != nil || y != nil {
		t.Fatalf("identical sets must have no difference: %v %v", x, y)
	}
}

func TestRules(t *testing.T) {
	one, none := []string{"x"}, []string(nil)
	many := []string{"a", "b", "c"}
	cases := []struct {
		name         string
		a, b         []string
		k            int
		swap, strict bool
	}{
		{"identical", none, none, 2, false, false},
		{"one word swapped", one, one, 2, true, true},
		{"only one side has an extra word", one, none, 2, false, true},
		{"reworded question, many differences", many, many, 2, false, true},
		{"two swapped, k=1", []string{"a", "b"}, one, 1, false, true},
		{"two swapped, k=2", []string{"a", "b"}, one, 2, true, true},
	}
	for _, c := range cases {
		if got := SwapReject(c.a, c.b, c.k); got != c.swap {
			t.Errorf("%s: SwapReject=%v want %v", c.name, got, c.swap)
		}
		if got := StrictReject(c.a, c.b); got != c.strict {
			t.Errorf("%s: StrictReject=%v want %v", c.name, got, c.strict)
		}
	}
}

func TestStopwordDifferenceIsInvisible(t *testing.T) {
	// "ship to Canada" vs "ship from Canada": only stopwords differ, so the
	// keyword check cannot tell them apart. The report relies on this being honest.
	x, y := Diff(ContentWords("Do you ship to Canada?", nil), ContentWords("Do you ship from Canada?", nil))
	if len(x) != 0 || len(y) != 0 {
		t.Fatalf("expected no visible difference, got %v %v", x, y)
	}
}

func TestEvaluate(t *testing.T) {
	should := []bool{true, true, false, false}
	served := []bool{true, false, true, false}
	c := Evaluate(should, func(i int) bool { return served[i] })
	if c.TrueHits != 1 || c.FalseMisses != 1 || c.FalseHits != 1 || c.TrueMisses != 1 {
		t.Fatalf("%+v", c)
	}
	if c.Precision == nil || *c.Precision != 0.5 || *c.Recall != 0.5 || c.F1 != 0.5 {
		t.Fatalf("%+v", c)
	}
	none := Evaluate(should, func(int) bool { return false })
	if none.Precision != nil || none.F1 != 0 {
		t.Fatalf("serving nothing has no precision: %+v", none)
	}
}

func TestBestThreshold(t *testing.T) {
	sims := []float64{0.9, 0.85, 0.7, 0.6}
	should := []bool{true, true, false, false}
	th, c := BestThreshold(sims, should, Grid())
	if c.F1 != 1 {
		t.Fatalf("F1=%v at %v", c.F1, th)
	}
	if th < 0.72 || th > 0.85 {
		t.Fatalf("threshold %v does not separate the groups", th)
	}
}

func TestBestThresholdTieGoesToLower(t *testing.T) {
	// Nothing is ever served correctly: F1 is 0 everywhere; pick the lowest.
	th, _ := BestThreshold([]float64{0.1, 0.9}, []bool{true, false}, Grid())
	if th != 0 {
		t.Fatalf("got %v", th)
	}
}

func TestRecallThreshold(t *testing.T) {
	sims := []float64{0.9, 0.5, 0.3}
	should := []bool{true, true, true}
	th, ok := RecallThreshold(sims, should, Grid(), 0.66)
	if !ok || th != 0.5 {
		t.Fatalf("got %v %v", th, ok)
	}
	if _, ok := RecallThreshold([]float64{-0.5}, []bool{true}, Grid(), 1); ok {
		t.Fatal("should not reach recall when the score is below the lowest threshold")
	}
}

func TestGrid(t *testing.T) {
	g := Grid()
	if len(g) != 51 || g[0] != 0 || g[40] != 0.8 || g[50] != 1 {
		t.Fatalf("grid wrong: len=%d %v %v %v", len(g), g[0], g[40], g[50])
	}
}

func TestVecLiteral(t *testing.T) {
	if got := vecLiteral([]float32{1, 0.5, -0.25}); got != "[1,0.5,-0.25]" {
		t.Fatal(got)
	}
}
