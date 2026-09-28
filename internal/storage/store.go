package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Pragmas apply per connection, which foreign_keys requires. _txlock=immediate:
// a deferred transaction upgrading read to write under WAL fails with
// BUSY_SNAPSHOT, which busy_timeout does not retry.
const (
	pragmas = "_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	maxConns = 4
)

type Store struct {
	db *sqlx.DB
	tx *sqlx.Tx

	Device *DeviceRepo
	Backup *BackupRepo
}

// Open opens the catalog at dsn and migrates it to the current schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	db, err := sqlx.Open("sqlite", dsn+separator+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	s.wireRepos()
	return s, nil
}

func migrate(ctx context.Context, db *sqlx.DB) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.DB, dir)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) wireRepos() {
	s.Device = &DeviceRepo{s: s}
	s.Backup = &BackupRepo{s: s}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) ext() sqlx.ExtContext {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// Nested calls join the outer transaction. Connections use _txlock=immediate,
// so a transaction takes the write lock up front: wrap writes, never plain reads.
func (s *Store) WithTx(ctx context.Context, fn func(*Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return wrap(err, "begin transaction")
	}
	defer func() { _ = tx.Rollback() }()

	txStore := &Store{db: s.db, tx: tx}
	txStore.wireRepos()

	if err := fn(txStore); err != nil {
		return err
	}
	return wrap(tx.Commit(), "commit transaction")
}
