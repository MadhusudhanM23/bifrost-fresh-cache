package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// pageEditor changes the price in the demo pricing page and puts the original
// bytes back afterwards. It exists for the "a price changed" and "everyone asks
// at once" scenarios only.
//
// It is byte-safe on purpose: it never decodes or re-encodes the file (the real
// page contains an em dash, which a text-mode rewrite could mangle). It finds
// the ASCII digits after "$" inside the one line that carries
// id="pro-plan-price" and splices new ASCII digits into the original bytes.
//
// A copy of the original is also written to the OS temp directory before the
// first edit, so if this process dies in the middle of a scenario the next
// start-up can put the page back (see recoverDemoPage).
type pageEditor struct {
	path   string
	orig   []byte
	mode   os.FileMode
	start  int // first digit of the price in orig
	end    int // one past the last digit
	price  int // the price in orig
	backup string
}

var priceDigits = regexp.MustCompile(`\$([0-9]+)`)

const priceLineMarker = `id="pro-plan-price"`

func backupPathFor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sum := sha1.Sum([]byte(abs))
	return filepath.Join(os.TempDir(), fmt.Sprintf("dashboard-demo-page-%x.orig", sum[:6]))
}

func openPageEditor(path string) (*pageEditor, error) {
	if path == "" {
		return nil, errors.New("no demo page was given")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("the demo page could not be found at %s", path)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the demo page could not be read: %v", err)
	}
	idx := bytes.Index(orig, []byte(priceLineMarker))
	if idx < 0 {
		return nil, errors.New("the demo page has no pricing line (id=\"pro-plan-price\"), so it cannot be edited safely")
	}
	lineLen := bytes.IndexByte(orig[idx:], '\n')
	if lineLen < 0 {
		lineLen = len(orig) - idx
	}
	seg := orig[idx : idx+lineLen]
	m := priceDigits.FindSubmatchIndex(seg)
	if m == nil {
		return nil, errors.New("the demo page's pricing line has no $ price in it, so it cannot be edited safely")
	}
	price, err := strconv.Atoi(string(seg[m[2]:m[3]]))
	if err != nil {
		return nil, errors.New("the price on the demo page is not a plain whole number")
	}
	p := &pageEditor{
		path: path, orig: append([]byte(nil), orig...), mode: info.Mode().Perm(),
		start: idx + m[2], end: idx + m[3], price: price, backup: backupPathFor(path),
	}
	if err := os.WriteFile(p.backup, p.orig, 0o600); err != nil {
		return nil, fmt.Errorf("could not save a safety copy of the demo page: %v", err)
	}
	return p, nil
}

// setPrice writes the original bytes with only the price digits replaced.
func (p *pageEditor) setPrice(n int) error {
	if n < 0 {
		return errors.New("price cannot be negative")
	}
	digits := strconv.Itoa(n)
	edited := make([]byte, 0, len(p.orig)+len(digits))
	edited = append(edited, p.orig[:p.start]...)
	edited = append(edited, digits...)
	edited = append(edited, p.orig[p.end:]...)
	return p.write(edited)
}

// write replaces the file's content in place (no rename: a file or folder
// shared into a container can stop seeing a renamed-over file). It overwrites
// without truncating first and trims only afterwards, so a same-length edit is
// never visible as an empty file.
func (p *pageEditor) write(b []byte) error {
	f, err := os.OpenFile(p.path, os.O_WRONLY, p.mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Truncate(int64(len(b))); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// restore puts the original bytes back, checks them, and only then removes the
// safety copy. If anything fails the safety copy stays, and the error names it.
func (p *pageEditor) restore() error {
	if err := p.write(p.orig); err != nil {
		return fmt.Errorf("could not put the demo page back: %v (the original is saved at %s)", err, p.backup)
	}
	got, err := os.ReadFile(p.path)
	if err != nil || !bytes.Equal(got, p.orig) {
		return fmt.Errorf("the demo page does not match its original after restoring (the original is saved at %s)", p.backup)
	}
	_ = os.Remove(p.backup)
	return nil
}

// recoverDemoPage runs once at start-up. A leftover safety copy means an
// earlier run died between editing and restoring, so the page is put back.
func recoverDemoPage(path string) {
	if path == "" {
		return
	}
	backup := backupPathFor(path)
	orig, err := os.ReadFile(backup)
	if err != nil {
		return
	}
	cur, err := os.ReadFile(path)
	if err == nil && bytes.Equal(cur, orig) {
		_ = os.Remove(backup)
		return
	}
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		log.Printf("demo page: an earlier run left it edited and it could not be restored: %v (original at %s)", err, backup)
		return
	}
	_ = os.Remove(backup)
	log.Printf("demo page: restored the original from an earlier interrupted run")
}
