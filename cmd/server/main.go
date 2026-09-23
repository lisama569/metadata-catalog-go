// Command server runs the metadata catalog service locally.
package main

import (
	"flag"
	"log"

	"github.com/lisama569/metadata-catalog-go/internal/catalog"
	"github.com/lisama569/metadata-catalog-go/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	journal := flag.String("journal", "catalog.jsonl", "journal file path (empty = in-memory)")
	allowRemoval := flag.Bool("allow-field-removal", false, "treat field removal as a warning")
	flag.Parse()

	cat, err := catalog.Open(catalog.Config{JournalPath: *journal, AllowFieldRemoval: *allowRemoval})
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	log.Printf("metadata catalog listening on %s (journal=%s)", *addr, *journal)
	if err := server.New(cat).ListenAndServe(*addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}
