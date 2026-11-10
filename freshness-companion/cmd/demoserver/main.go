// Command demoserver serves demo-page/ locally with caching disabled, so an
// edit to the price is visible to the companion's direct provider at once.
// It is only for offline demos; the published demo page lives on GitHub Pages.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8000", "listen address")
	dir := flag.String("dir", "demo-page", "directory to serve")
	flag.Parse()

	files := http.FileServer(http.Dir(*dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("serving %s at http://%s/", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
