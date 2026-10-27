package main

import (
	"path/filepath"
	"testing"
)

// ServeMux panics on conflicting patterns; catch that in tests, not at startup.
func TestRoutesRegister(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp(db, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	app.routes()
}
