package main

import (
	"embed"
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	dbPath := flag.String("db", envOr("DB_PATH", "family.db"), "SQLite database file")
	uploads := flag.String("uploads", envOr("UPLOADS_DIR", "uploads"), "directory for uploaded documents")
	secure := flag.Bool("secure-cookies", os.Getenv("SECURE_COOKIES") == "1", "mark cookies Secure (enable when served over HTTPS)")
	flag.Parse()

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := os.MkdirAll(*uploads, 0o700); err != nil {
		log.Fatalf("create uploads directory: %v", err)
	}

	app, err := newApp(db, *uploads, *secure)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
