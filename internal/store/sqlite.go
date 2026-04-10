package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating store directory: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening store database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening store database: %w", err)
	}

	const ddl = `CREATE TABLE IF NOT EXISTS runs (
		id             TEXT PRIMARY KEY,
		repo           TEXT NOT NULL,
		ticket         TEXT NOT NULL,
		branch         TEXT NOT NULL DEFAULT '',
		workflow       TEXT NOT NULL DEFAULT '',
		provider       TEXT NOT NULL,
		instance_id    TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL,
		exit_code      INTEGER,
		launched_by    TEXT NOT NULL,
		started_at     TEXT NOT NULL,
		completed_at   TEXT,
		timeout_at     TEXT NOT NULL,
		total_cost_usd REAL
	);`

	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating store schema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
