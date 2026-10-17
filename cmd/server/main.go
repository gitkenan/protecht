// Command server runs the protecht backend: the HTTP API plus the HTML
// activity timeline. It binds to localhost by default.
package main

import (
	"flag"
	"log"
	"net/http"

	"protecht/internal/api"
	"protecht/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dbPath := flag.String("db", "protecht.db", "SQLite database file path")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	log.Printf("protecht server listening on http://%s (db: %s)", *addr, *dbPath)
	log.Printf("activity timeline: http://%s/", *addr)
	log.Fatal(http.ListenAndServe(*addr, api.NewServer(st)))
}
