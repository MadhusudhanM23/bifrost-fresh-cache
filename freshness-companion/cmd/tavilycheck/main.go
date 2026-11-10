// Command tavilycheck reads one public page through the companion's own
// Tavily Extract code, to prove a TAVILY_API_KEY works before a demo.
//
//	TAVILY_API_KEY must be set in the environment (it is never printed).
//	tavilycheck -url https://www.tavily.com/pricing -find credits
//
// Each run uses about 1 Tavily credit (basic Extract: 1 credit per 5 pages).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"freshness-companion/internal/evidence"
)

func main() {
	url := flag.String("url", "https://example.com/", "public page to read through Tavily Extract")
	find := flag.String("find", "", "optional text that should appear in the page")
	timeout := flag.Duration("timeout", 30*time.Second, "overall time limit")
	flag.Parse()

	p, err := evidence.NewTavilyExtract(evidence.TavilyConfig{
		APIKey:     os.Getenv("TAVILY_API_KEY"),
		HTTPClient: &http.Client{Timeout: *timeout},
	})
	if err != nil {
		fmt.Println("FAIL ", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	start := time.Now()
	ev, err := p.Fetch(ctx, *url)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		// Errors from the Tavily client have the key redacted.
		fmt.Printf("FAIL  Tavily Extract of %s after %s: %v\n", *url, took, err)
		os.Exit(1)
	}
	snippet := strings.Join(strings.Fields(ev.Content), " ")
	if len(snippet) > 160 {
		snippet = snippet[:160] + "..."
	}
	fmt.Printf("PASS  Tavily Extract read %s: %d characters in %s (provider %s)\n", *url, len(ev.Content), took, ev.Provider)
	fmt.Printf("      starts: %s\n", snippet)
	if *find != "" {
		if strings.Contains(strings.ToLower(ev.Content), strings.ToLower(*find)) {
			fmt.Printf("PASS  the page contains %q\n", *find)
		} else {
			fmt.Printf("FAIL  the page does not contain %q\n", *find)
			os.Exit(1)
		}
	}
}
