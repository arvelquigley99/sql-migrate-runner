# sql-migrate-runner

A small CLI that reads SQL files from a directory and runs them in order
against a database, tracking what has been applied in a `schema_migrations`
table.

I wrote this because I kept seeing teams run migrations by hand with psql
and a spreadsheet, or pay for a migration tool that did more than they
needed. This does the boring part: files in, table updated, transaction per
migration, roll back when you need to.

## File naming

```
migrations/
  001_create_users.up.sql
  001_create_users.down.sql
  002_add_email_index.up.sql
  002_add_email_index.down.sql
```

The version prefix (before the first dot) determines order. `.up.sql` runs
forward, `.down.sql` rolls back.

## Usage

```bash
# Apply all pending migrations
sql-migrate-runner --dsn "postgres://user:pass@localhost:5432/app?sslmode=disable"

# Roll back the last 2
sql-migrate-runner --dsn "..." --down 2

# Custom migrations directory
sql-migrate-runner --dir ./db/migrations --dsn "..."

# SQLite (register your own driver)
sql-migrate-runner --driver sqlite3 --dsn "file:./app.db"
```

## How it works

- Each migration runs in its own transaction. If it fails, the transaction
  rolls back and the runner stops — earlier migrations stay committed.
- A `schema_migrations` table tracks which versions have been applied.
- `--down N` finds the N most recent applied migrations and runs their
  `.down.sql` counterparts in reverse order.

## Notes

This tool uses `database/sql` with a configurable `--driver` flag. You need
to register the driver you want to use — for Postgres, add
`import _ "github.com/lib/pq"` to your build. For SQLite,
`import _ "modernc.org/sqlite"` (pure Go) or `github.com/mattn/go-sqlite3`.

The default `--driver` is `postgres` because that is what I run.

## License

MIT
