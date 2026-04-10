package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
		metadata       TEXT,
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

func (s *SQLiteStore) CreateRun(ctx context.Context, run *Run) error {
	var metadataStr *string
	if run.Metadata != nil {
		b, err := json.Marshal(run.Metadata)
		if err != nil {
			return fmt.Errorf("marshaling run metadata: %w", err)
		}
		str := string(b)
		metadataStr = &str
	}

	var completedAt *string
	if run.CompletedAt != nil {
		completedAtStr := run.CompletedAt.Format(time.RFC3339)
		completedAt = &completedAtStr
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (
			id, repo, ticket, branch, workflow, provider,
			instance_id, metadata, status, exit_code, launched_by,
			started_at, completed_at, timeout_at, total_cost_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID,
		run.Repo,
		run.Ticket,
		run.Branch,
		run.Workflow,
		run.Provider,
		run.InstanceID,
		metadataStr,
		string(run.Status),
		run.ExitCode,
		run.LaunchedBy,
		run.StartedAt.Format(time.RFC3339),
		completedAt,
		run.TimeoutAt.Format(time.RFC3339),
		run.TotalCostUSD,
	)
	if err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo, ticket, branch, workflow, provider,
			instance_id, metadata, status, exit_code, launched_by,
			started_at, completed_at, timeout_at, total_cost_usd
		FROM runs WHERE id = ?`, id)

	var run Run
	var metadataStr sql.NullString
	var status string
	var exitCode sql.NullInt64
	var startedAt string
	var completedAt sql.NullString
	var timeoutAt string
	var totalCostUSD sql.NullFloat64

	err := row.Scan(
		&run.ID,
		&run.Repo,
		&run.Ticket,
		&run.Branch,
		&run.Workflow,
		&run.Provider,
		&run.InstanceID,
		&metadataStr,
		&status,
		&exitCode,
		&run.LaunchedBy,
		&startedAt,
		&completedAt,
		&timeoutAt,
		&totalCostUSD,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("scanning run: %w", err)
	}

	run.Status = Status(status)

	if exitCode.Valid {
		v := int(exitCode.Int64)
		run.ExitCode = &v
	}

	var parseErr error
	run.StartedAt, parseErr = time.Parse(time.RFC3339, startedAt)
	if parseErr != nil {
		return nil, fmt.Errorf("parsing started_at: %w", parseErr)
	}

	if completedAt.Valid {
		t, parseErr := time.Parse(time.RFC3339, completedAt.String)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing completed_at: %w", parseErr)
		}
		run.CompletedAt = &t
	}

	run.TimeoutAt, parseErr = time.Parse(time.RFC3339, timeoutAt)
	if parseErr != nil {
		return nil, fmt.Errorf("parsing timeout_at: %w", parseErr)
	}

	if totalCostUSD.Valid {
		run.TotalCostUSD = &totalCostUSD.Float64
	}

	if metadataStr.Valid {
		if err := json.Unmarshal([]byte(metadataStr.String), &run.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshaling run metadata: %w", err)
		}
	}

	return &run, nil
}
