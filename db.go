package main

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS families (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	invite_code TEXT NOT NULL UNIQUE,
	created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	email         TEXT NOT NULL UNIQUE,
	name          TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	family_id     INTEGER NOT NULL REFERENCES families(id),
	created_at    INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS shopping_items (
	id         INTEGER PRIMARY KEY,
	family_id  INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	quantity   TEXT NOT NULL DEFAULT '',
	done       INTEGER NOT NULL DEFAULT 0,
	not_found  INTEGER NOT NULL DEFAULT 0,
	photo      TEXT NOT NULL DEFAULT '', -- file in <uploads>/photos, '' for none
	added_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS shopping_items_family ON shopping_items(family_id);

CREATE TABLE IF NOT EXISTS todos (
	id         INTEGER PRIMARY KEY,
	family_id  INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	title      TEXT NOT NULL,
	done       INTEGER NOT NULL DEFAULT 0,
	added_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS todos_family ON todos(family_id);

CREATE TABLE IF NOT EXISTS documents (
	id          INTEGER PRIMARY KEY,
	family_id   INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	stored_name TEXT NOT NULL UNIQUE, -- file name inside the uploads directory
	size        INTEGER NOT NULL,
	added_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS documents_family ON documents(family_id);

CREATE TABLE IF NOT EXISTS favorites (
	id        INTEGER PRIMARY KEY,
	family_id INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	name      TEXT NOT NULL COLLATE NOCASE,
	quantity  TEXT NOT NULL DEFAULT '',
	UNIQUE (family_id, name)
);

-- A finished shopping trip and what was bought (or not found) on it.
CREATE TABLE IF NOT EXISTS shopping_trips (
	id          INTEGER PRIMARY KEY,
	family_id   INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	finished_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	finished_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS shopping_trips_family ON shopping_trips(family_id);

CREATE TABLE IF NOT EXISTS shopping_trip_items (
	id       INTEGER PRIMARY KEY,
	trip_id  INTEGER NOT NULL REFERENCES shopping_trips(id) ON DELETE CASCADE,
	name     TEXT NOT NULL,
	quantity TEXT NOT NULL DEFAULT '',
	status   TEXT NOT NULL, -- 'bought' or 'not_found'
	photo    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS shopping_trip_items_trip ON shopping_trip_items(trip_id);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- One row per device that turned notifications on.
CREATE TABLE IF NOT EXISTS push_subscriptions (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	endpoint   TEXT NOT NULL UNIQUE,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- A row means that class of the Kinder Schwimmschule course is done.
CREATE TABLE IF NOT EXISTS swim_classes (
	family_id INTEGER NOT NULL REFERENCES families(id) ON DELETE CASCADE,
	number    INTEGER NOT NULL,
	done_at   INTEGER NOT NULL DEFAULT (unixepoch()),
	done_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	PRIMARY KEY (family_id, number)
);
`

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection avoids "database is locked" errors.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Columns added after a table was first created. CREATE TABLE IF NOT EXISTS
// doesn't touch existing tables, so older databases get them here.
var addedColumns = []struct{ table, column, definition string }{
	{"shopping_items", "not_found", "INTEGER NOT NULL DEFAULT 0"},
	{"shopping_items", "photo", "TEXT NOT NULL DEFAULT ''"},
	{"shopping_trip_items", "photo", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.definition); err != nil {
				return err
			}
		}
	}
	return nil
}
