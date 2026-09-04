package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Migration is a single SQL file to apply (or roll back).
type Migration struct {
	Version   string // filename prefix, e.g. "001"
	Direction string // "up" or "down"
	Source    string // raw SQL
}

// Migrator runs migrations against a *sql.DB and tracks what has been applied
// in a schema_migrations table.
type Migrator struct {
	db  *sql.DB
	log *slog.Logger
}

func New(db *sql.DB, log *slog.Logger) *Migrator {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Migrator{db: db, log: log}
}

// ensureTable creates the tracking table if it does not already exist.
func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version   TEXT PRIMARY KEY,
			direction TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

// appliedVersions returns the set of "up" migrations that have already run.
func (m *Migrator) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT version FROM schema_migrations WHERE direction = 'up'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// LoadMigrations reads .sql files from dir. Filenames must be
// {version}.{direction}.sql, e.g. 001_create_users.up.sql.
func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		parts := strings.SplitN(strings.TrimSuffix(e.Name(), ".sql"), ".", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected filename %s; expected {version}.{direction}.sql", e.Name())
		}
		out = append(out, Migration{Version: parts[0], Direction: parts[1], Source: string(content)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Up runs all pending "up" migrations in version order, each in its own
// transaction. Stops on the first error — earlier migrations stay committed.
func (m *Migrator) Up(ctx context.Context, dir string) error {
	if err := m.ensureTable(ctx); err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}
	all, err := LoadMigrations(dir)
	if err != nil {
		return err
	}
	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("query applied: %w", err)
	}
	pending := 0
	for _, mg := range all {
		if mg.Direction != "up" || applied[mg.Version] {
			continue
		}
		if err := m.runOne(ctx, mg); err != nil {
			return fmt.Errorf("migration %s: %w", mg.Version, err)
		}
		m.log.Info("applied", "version", mg.Version)
		pending++
	}
	if pending == 0 {
		m.log.Info("nothing to apply")
	}
	return nil
}

// Down rolls back the most recent N applied migrations. Each down migration
// runs in a transaction and removes the corresponding "up" record.
func (m *Migrator) Down(ctx context.Context, dir string, n int) error {
	if err := m.ensureTable(ctx); err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}
	all, err := LoadMigrations(dir)
	if err != nil {
		return err
	}
	downMap := make(map[string]string)
	var upVersions []string
	for _, mg := range all {
		if mg.Direction == "down" {
			downMap[mg.Version] = mg.Source
		} else {
			upVersions = append(upVersions, mg.Version)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(upVersions)))

	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("query applied: %w", err)
	}
	rolled := 0
	for _, v := range upVersions {
		if !applied[v] {
			continue
		}
		source, ok := downMap[v]
		if !ok {
			return fmt.Errorf("no down migration for version %s", v)
		}
		mg := Migration{Version: v, Direction: "down", Source: source}
		if err := m.runOne(ctx, mg); err != nil {
			return fmt.Errorf("rollback %s: %w", v, err)
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM schema_migrations WHERE version = $1 AND direction = 'up'`, v); err != nil {
			return fmt.Errorf("delete migration record %s: %w", v, err)
		}
		m.log.Info("rolled back", "version", v)
		rolled++
		if n > 0 && rolled >= n {
			break
		}
	}
	if rolled == 0 {
		m.log.Info("nothing to roll back")
	}
	return nil
}

// runOne executes a migration inside a transaction and records it.
func (m *Migrator) runOne(ctx context.Context, mg Migration) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if strings.TrimSpace(mg.Source) != "" {
		if _, err := tx.ExecContext(ctx, mg.Source); err != nil {
			return fmt.Errorf("exec SQL: %w", err)
		}
	}
	if mg.Direction == "up" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, direction) VALUES ($1, 'up')`,
			mg.Version); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
	}
	return tx.Commit()
}

func main() {
	var (
		dir    = flag.String("dir", "migrations", "path to migration files")
		driver = flag.String("driver", "postgres", "database/sql driver name")
		dsn    = flag.String("dsn", "", "database connection string")
		down   = flag.Int("down", 0, "roll back N migrations (0 = run up)")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "error: --dsn is required")
		os.Exit(1)
	}

	db, err := sql.Open(*driver, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m := New(db, nil)

	if *down > 0 {
		if err := m.Down(ctx, *dir, *down); err != nil {
			fmt.Fprintf(os.Stderr, "down: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := m.Up(ctx, *dir); err != nil {
			fmt.Fprintf(os.Stderr, "up: %v\n", err)
			os.Exit(1)
		}
	}
}
