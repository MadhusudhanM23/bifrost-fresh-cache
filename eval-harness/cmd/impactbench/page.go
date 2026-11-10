package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
)

// priceSentence finds the number in the demo page's price sentence
// ("Acme Pro plan — $49 per month"). Matching is on bytes: the page holds an
// em dash (three UTF-8 bytes) that must come through untouched.
var priceSentence = regexp.MustCompile(`(Acme Pro plan[^$0-9]{0,20}\$)([0-9]+)( per month)`)

// PriceOf reports the price the page states.
func PriceOf(page []byte) (int, error) {
	m := priceSentence.FindAllSubmatch(page, -1)
	if len(m) != 1 {
		return 0, fmt.Errorf("want exactly one price sentence on the page, found %d", len(m))
	}
	return strconv.Atoi(string(m[0][2]))
}

// WithPrice returns a copy of page whose price sentence states price. Only the
// digits of the price change; every other byte is kept.
func WithPrice(page []byte, price int) ([]byte, error) {
	if _, err := PriceOf(page); err != nil {
		return nil, err
	}
	if price < 0 {
		return nil, fmt.Errorf("price must not be negative")
	}
	loc := priceSentence.FindSubmatchIndex(page)
	start, end := loc[4], loc[5] // the digits (group 2)
	out := make([]byte, 0, len(page)+8)
	out = append(out, page[:start]...)
	out = append(out, strconv.Itoa(price)...)
	out = append(out, page[end:]...)
	return out, nil
}

// Page is the demo source page, edited in place and always restorable. It
// remembers the exact original bytes and writes them back on Restore.
type Page struct {
	path    string
	orig    []byte
	origSHA string

	mu      sync.Mutex
	touched bool
}

// OpenPage reads the page's current bytes as the original.
func OpenPage(path string) (*Page, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &Page{path: path, orig: b, origSHA: hex.EncodeToString(sum[:])}, nil
}

// OriginalSHA256 and OriginalSize describe the bytes Restore writes back.
func (p *Page) OriginalSHA256() string { return p.origSHA }
func (p *Page) OriginalSize() int      { return len(p.orig) }

// BasePrice is the price in the original bytes.
func (p *Page) BasePrice() (int, error) { return PriceOf(p.orig) }

// Set rewrites the page so it states price (always derived from the original
// bytes, never from an earlier edit).
func (p *Page) Set(price int) error {
	edited, err := WithPrice(p.orig, price)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touched = true
	return os.WriteFile(p.path, edited, 0o644)
}

// Restore writes the original bytes back and checks them by reading the file
// again. It is safe to call more than once and from several goroutines.
func (p *Page) Restore() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.WriteFile(p.path, p.orig, 0o644); err != nil {
		return fmt.Errorf("restoring %s: %w", p.path, err)
	}
	p.touched = false
	return p.verifyLocked()
}

// Verify checks that the file holds exactly the original bytes.
func (p *Page) Verify() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifyLocked()
}

func (p *Page) verifyLocked() error {
	b, err := os.ReadFile(p.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, p.orig) {
		return fmt.Errorf("%s differs from the original (%d bytes now, %d originally)", p.path, len(b), len(p.orig))
	}
	return nil
}
