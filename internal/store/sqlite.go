package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

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

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL journal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting busy timeout: %w", err)
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
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing store database: %w", err)
	}
	return nil
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
		completedAtStr := run.CompletedAt.UTC().Format(time.RFC3339)
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
		run.StartedAt.UTC().Format(time.RFC3339),
		completedAt,
		run.TimeoutAt.UTC().Format(time.RFC3339),
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

	run, err := s.scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
		}
		return nil, err
	}
	return run, nil
}

// scanRun scans a single row from the runs table into a *Run.
// The row must contain all 15 columns in the standard SELECT order.
func (s *SQLiteStore) scanRun(scanner interface{ Scan(dest ...any) error }) (*Run, error) {
	var run Run
	var metadataStr sql.NullString
	var status string
	var exitCode sql.NullInt64
	var startedAt string
	var completedAt sql.NullString
	var timeoutAt string
	var totalCostUSD sql.NullFloat64

	if err := scanner.Scan(
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
	); err != nil {
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

func (s *SQLiteStore) UpdateRun(ctx context.Context, id string, update *RunUpdate) error {
	var setClauses []string
	var args []any

	if update.Status != nil {
		setClauses = append(setClauses, "status = ?")
		args = append(args, string(*update.Status))
	}
	if update.InstanceID != nil {
		setClauses = append(setClauses, "instance_id = ?")
		args = append(args, *update.InstanceID)
	}
	if update.Metadata != nil {
		b, err := json.Marshal(update.Metadata)
		if err != nil {
			return fmt.Errorf("marshaling run metadata: %w", err)
		}
		setClauses = append(setClauses, "metadata = ?")
		args = append(args, string(b))
	}
	if update.ExitCode != nil {
		setClauses = append(setClauses, "exit_code = ?")
		args = append(args, *update.ExitCode)
	}
	if update.CompletedAt != nil {
		setClauses = append(setClauses, "completed_at = ?")
		args = append(args, update.CompletedAt.UTC().Format(time.RFC3339))
	}
	if update.TotalCostUSD != nil {
		setClauses = append(setClauses, "total_cost_usd = ?")
		args = append(args, *update.TotalCostUSD)
	}
	if update.TimeoutAt != nil {
		setClauses = append(setClauses, "timeout_at = ?")
		args = append(args, update.TimeoutAt.UTC().Format(time.RFC3339))
	}

	if len(setClauses) == 0 {
		var exists bool
		err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id = ?)", id).Scan(&exists)
		if err != nil {
			return fmt.Errorf("checking run existence: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrRunNotFound, id)
		}
		return nil
	}

	query := "UPDATE runs SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	args = append(args, id)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("updating run: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating run: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return nil
}

func (s *SQLiteStore) ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error) {
	query := `SELECT id, repo, ticket, branch, workflow, provider,
		instance_id, metadata, status, exit_code, launched_by,
		started_at, completed_at, timeout_at, total_cost_usd
		FROM runs WHERE repo = ?`
	args := []any{repo}

	if activeOnly {
		query += " AND status IN ('pending', 'running')"
	}
	query += " ORDER BY started_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing runs by repo: %w", err)
	}
	defer rows.Close()

	runs := make([]*Run, 0)
	for rows.Next() {
		run, err := s.scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("listing runs by repo: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runs by repo: %w", err)
	}
	return runs, nil
}

func (s *SQLiteStore) FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo, ticket, branch, workflow, provider,
			instance_id, metadata, status, exit_code, launched_by,
			started_at, completed_at, timeout_at, total_cost_usd
		FROM runs WHERE repo = ? AND ticket = ? AND status IN ('pending', 'running')
		ORDER BY started_at DESC`,
		repo, ticket)
	if err != nil {
		return nil, fmt.Errorf("finding active runs by ticket: %w", err)
	}
	defer rows.Close()

	runs := make([]*Run, 0)
	for rows.Next() {
		run, err := s.scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("finding active runs by ticket: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding active runs by ticket: %w", err)
	}
	return runs, nil
}

func (s *SQLiteStore) CountActive(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE status IN ('pending', 'running')").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting active runs: %w", err)
	}
	return count, nil
}
