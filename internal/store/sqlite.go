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

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

// runColumns is the canonical column list for every runs SELECT, in the order
// scanRun expects. Defined once so the column set can grow without drifting
// across the several queries that share scanRun.
const runColumns = `id, repo, ticket, branch, workflow, provider,
	instance_id, metadata, labels, status, exit_code, launched_by,
	started_at, completed_at, timeout_at, total_cost_usd,
	input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, turns,
	enqueued_at, priority`

func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating store directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
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
		labels         TEXT,
		status         TEXT NOT NULL,
		exit_code      INTEGER,
		launched_by    TEXT NOT NULL,
		started_at     TEXT NOT NULL,
		completed_at   TEXT,
		timeout_at     TEXT NOT NULL,
		total_cost_usd REAL,
		input_tokens          INTEGER,
		output_tokens         INTEGER,
		cache_creation_tokens INTEGER,
		cache_read_tokens     INTEGER,
		turns                 INTEGER,
		enqueued_at    TEXT,
		priority       TEXT NOT NULL DEFAULT ''
	);`

	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating store schema: %w", err)
	}

	if err := ensureColumns(db); err != nil {
		db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

// ensureColumns brings an existing on-disk schema up to date with columns added
// after the original CREATE TABLE. SQLite has no migration framework here; this
// is an idempotent ALTER-if-missing so pre-existing ~/.horde/horde.db files
// (created before a column was added) keep working. Each entry is additive and
// safe to run on every open.
func ensureColumns(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(runs)")
	if err != nil {
		return fmt.Errorf("inspecting store schema: %w", err)
	}
	defer rows.Close()

	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &primaryKey); err != nil {
			return fmt.Errorf("inspecting store schema: %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspecting store schema: %w", err)
	}

	// name -> "ALTER TABLE runs ADD COLUMN" type. Additive only.
	additive := []struct{ name, ddl string }{
		{"labels", "TEXT"},
		{"input_tokens", "INTEGER"},
		{"output_tokens", "INTEGER"},
		{"cache_creation_tokens", "INTEGER"},
		{"cache_read_tokens", "INTEGER"},
		{"turns", "INTEGER"},
		{"enqueued_at", "TEXT"},
		{"priority", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, col := range additive {
		if existing[col.name] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE runs ADD COLUMN %s %s", col.name, col.ddl)); err != nil {
			return fmt.Errorf("adding %q column: %w", col.name, err)
		}
	}
	return nil
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

	var labelsStr *string
	if run.Labels != nil {
		b, err := json.Marshal(run.Labels)
		if err != nil {
			return fmt.Errorf("marshaling run labels: %w", err)
		}
		str := string(b)
		labelsStr = &str
	}

	var completedAt *string
	if run.CompletedAt != nil {
		completedAtStr := run.CompletedAt.UTC().Format(time.RFC3339)
		completedAt = &completedAtStr
	}

	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, turns *int
	if run.Tokens != nil {
		inputTokens = &run.Tokens.InputTokens
		outputTokens = &run.Tokens.OutputTokens
		cacheCreationTokens = &run.Tokens.CacheCreationTokens
		cacheReadTokens = &run.Tokens.CacheReadTokens
		turns = &run.Tokens.Turns
	}

	var enqueuedAt *string
	if !run.EnqueuedAt.IsZero() {
		enqueuedAtStr := run.EnqueuedAt.UTC().Format(time.RFC3339)
		enqueuedAt = &enqueuedAtStr
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (
			id, repo, ticket, branch, workflow, provider,
			instance_id, metadata, labels, status, exit_code, launched_by,
			started_at, completed_at, timeout_at, total_cost_usd,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, turns,
			enqueued_at, priority
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID,
		run.Repo,
		run.Ticket,
		run.Branch,
		run.Workflow,
		run.Provider,
		run.InstanceID,
		metadataStr,
		labelsStr,
		string(run.Status),
		run.ExitCode,
		run.LaunchedBy,
		run.StartedAt.UTC().Format(time.RFC3339),
		completedAt,
		run.TimeoutAt.UTC().Format(time.RFC3339),
		run.TotalCostUSD,
		inputTokens,
		outputTokens,
		cacheCreationTokens,
		cacheReadTokens,
		turns,
		enqueuedAt,
		string(run.Priority),
	)
	if err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+`
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
// The row must contain the runColumns set in that exact order.
func (s *SQLiteStore) scanRun(scanner interface{ Scan(dest ...any) error }) (*Run, error) {
	var run Run
	var metadataStr sql.NullString
	var labelsStr sql.NullString
	var status string
	var exitCode sql.NullInt64
	var startedAt string
	var completedAt sql.NullString
	var timeoutAt string
	var totalCostUSD sql.NullFloat64
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, turns sql.NullInt64
	var enqueuedAt sql.NullString
	var priority string

	if err := scanner.Scan(
		&run.ID,
		&run.Repo,
		&run.Ticket,
		&run.Branch,
		&run.Workflow,
		&run.Provider,
		&run.InstanceID,
		&metadataStr,
		&labelsStr,
		&status,
		&exitCode,
		&run.LaunchedBy,
		&startedAt,
		&completedAt,
		&timeoutAt,
		&totalCostUSD,
		&inputTokens,
		&outputTokens,
		&cacheCreationTokens,
		&cacheReadTokens,
		&turns,
		&enqueuedAt,
		&priority,
	); err != nil {
		return nil, fmt.Errorf("scanning run: %w", err)
	}

	run.Status = Status(status)
	run.Priority = Priority(priority)

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

	if enqueuedAt.Valid {
		t, parseErr := time.Parse(time.RFC3339, enqueuedAt.String)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing enqueued_at: %w", parseErr)
		}
		run.EnqueuedAt = t
	}

	if inputTokens.Valid || outputTokens.Valid || cacheCreationTokens.Valid || cacheReadTokens.Valid || turns.Valid {
		run.Tokens = &TokenUsage{
			InputTokens:         int(inputTokens.Int64),
			OutputTokens:        int(outputTokens.Int64),
			CacheCreationTokens: int(cacheCreationTokens.Int64),
			CacheReadTokens:     int(cacheReadTokens.Int64),
			Turns:               int(turns.Int64),
		}
	}

	if metadataStr.Valid {
		if err := json.Unmarshal([]byte(metadataStr.String), &run.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshaling run metadata: %w", err)
		}
	}

	if labelsStr.Valid {
		if err := json.Unmarshal([]byte(labelsStr.String), &run.Labels); err != nil {
			return nil, fmt.Errorf("unmarshaling run labels: %w", err)
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
	if update.Tokens != nil {
		setClauses = append(setClauses,
			"input_tokens = ?",
			"output_tokens = ?",
			"cache_creation_tokens = ?",
			"cache_read_tokens = ?",
			"turns = ?",
		)
		args = append(args,
			update.Tokens.InputTokens,
			update.Tokens.OutputTokens,
			update.Tokens.CacheCreationTokens,
			update.Tokens.CacheReadTokens,
			update.Tokens.Turns,
		)
	}
	if update.TimeoutAt != nil {
		setClauses = append(setClauses, "timeout_at = ?")
		args = append(args, update.TimeoutAt.UTC().Format(time.RFC3339))
	}
	if update.Priority != nil {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, string(*update.Priority))
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
	query := `SELECT ` + runColumns + `
		FROM runs WHERE repo = ?`
	args := []any{repo}

	if activeOnly {
		query += " AND status IN (?, ?)"
		args = append(args, string(StatusPending), string(StatusRunning))
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

// ListRuns fetches the repo-scoped rows (applying the started_at range in SQL,
// since it maps cleanly to indexed comparisons) and applies the remaining
// status/workflow/ticket/label predicates via the shared matchesFilter helper.
// SQLite is the local-testing store, so this is intentionally simple rather
// than pushing every predicate into SQL — the production query optimization
// lives in the DynamoDB store's server-side FilterExpression.
func (s *SQLiteStore) ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error) {
	query := `SELECT ` + runColumns + `
		FROM runs WHERE repo = ?`
	args := []any{filter.Repo}

	if filter.Since != nil {
		query += " AND started_at >= ?"
		args = append(args, filter.Since.UTC().Format(time.RFC3339))
	}
	if filter.Until != nil {
		query += " AND started_at <= ?"
		args = append(args, filter.Until.UTC().Format(time.RFC3339))
	}
	query += " ORDER BY started_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	defer rows.Close()

	runs := make([]*Run, 0)
	for rows.Next() {
		run, err := s.scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("listing runs: %w", err)
		}
		if matchesFilter(run, filter) {
			runs = append(runs, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	return runs, nil
}

func (s *SQLiteStore) FindActiveByTicket(ctx context.Context, repo, ticket, workflow string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+`
		FROM runs WHERE repo = ? AND ticket = ? AND workflow = ? AND status IN (?, ?, ?)
		ORDER BY started_at DESC`,
		repo, ticket, workflow, string(StatusPending), string(StatusRunning), string(StatusQueued))
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
		"SELECT COUNT(*) FROM runs WHERE status IN (?, ?)",
		string(StatusPending), string(StatusRunning)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting active runs: %w", err)
	}
	return count, nil
}

func (s *SQLiteStore) ListActive(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+`
		FROM runs WHERE status IN (?, ?)
		ORDER BY started_at DESC`,
		string(StatusPending), string(StatusRunning))
	if err != nil {
		return nil, fmt.Errorf("listing active runs: %w", err)
	}
	defer rows.Close()

	runs := make([]*Run, 0)
	for rows.Next() {
		run, err := s.scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("listing active runs: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing active runs: %w", err)
	}
	return runs, nil
}
