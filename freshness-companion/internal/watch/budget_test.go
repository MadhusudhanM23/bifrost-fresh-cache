package watch

import (
	"testing"
	"time"
)

func TestBudgetRollingHour(t *testing.T) {
	clock := newClock()
	b := NewBudget(3, clock.Now)
	for i := 0; i < 3; i++ {
		if !b.Take() {
			t.Fatalf("take %d should be allowed", i+1)
		}
		clock.Advance(10 * time.Minute)
	}
	if b.Take() {
		t.Fatal("the 4th take within the hour must be refused")
	}
	if b.Used() != 3 {
		t.Fatalf("used = %d, want 3", b.Used())
	}
	// 30 min have passed since the first take; at 60 min it leaves the window.
	clock.Advance(30*time.Minute + time.Second)
	if b.Used() != 2 {
		t.Fatalf("used after the first take aged out = %d, want 2", b.Used())
	}
	if !b.Take() {
		t.Fatal("one slot should be free again")
	}
	if b.Take() {
		t.Fatal("and only one")
	}
	if b.Max() != 3 {
		t.Fatalf("max = %d", b.Max())
	}
}

func TestBudgetZeroAllowsNothing(t *testing.T) {
	b := NewBudget(0, nil)
	if b.Take() {
		t.Fatal("a zero budget must refuse every take")
	}
	if NewBudget(-5, nil).Take() {
		t.Fatal("a negative budget means zero")
	}
}
