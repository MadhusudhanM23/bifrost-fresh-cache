package main

import (
	"path/filepath"
	"testing"
)

func TestParseThresholdsLandsOnTwoDecimalValues(t *testing.T) {
	levels, err := parseThresholds("0.70:0.98:0.02")
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 15 || levels[0] != 0.70 || levels[14] != 0.98 || levels[5] != 0.80 {
		t.Fatalf("got %v", levels)
	}
}

func TestConfusion(t *testing.T) {
	var c Confusion
	for _, d := range []struct{ shouldHit, hit bool }{{true, true}, {true, true}, {true, false}, {false, true}, {false, false}} {
		c.count(d.shouldHit, d.hit)
	}
	c.finish()
	if c.TrueHits != 2 || c.FalseHits != 1 || c.FalseMisses != 1 || c.TrueMisses != 1 {
		t.Fatalf("counts %+v", c)
	}
	if *c.Precision != 2.0/3 || *c.Recall != 2.0/3 || c.F1 < 0.6666 || c.F1 > 0.6667 {
		t.Fatalf("precision %v recall %v f1 %v", *c.Precision, *c.Recall, c.F1)
	}
	var none Confusion
	none.count(true, false)
	none.finish()
	if none.Precision != nil || none.F1 != 0 {
		t.Fatalf("no predicted hits: precision should be undefined and F1 0, got %+v", none)
	}
}

func TestDatasetIsValidAndBalanced(t *testing.T) {
	data, err := loadDataset(filepath.Join("..", "..", "datasets", "paraphrases.json"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, p := range data.Pairs {
		counts[p.Label]++
	}
	if len(data.Pairs) < 60 || counts["hit"] < 30 || counts["miss"] < 30 {
		t.Fatalf("dataset has %d pairs (%v); want at least 60 with both labels well represented", len(data.Pairs), counts)
	}
}

func TestEmbeddingTextMatchesThePlugin(t *testing.T) {
	if got := embeddingText("  Where is my ORDER? ", "chat"); got != "user: where is my order?" {
		t.Fatalf("got %q", got)
	}
}
