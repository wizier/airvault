// Package storage is the sqlx data-access layer: a Store owns one repo per
// aggregate, every repo method is a single statement, and callers compose the
// ones that must commit together with WithTx.
package storage

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// Store is the single dependency services hold for DB access.
type Store struct {
	db *sqlx.DB
	tx *sqlx.Tx // non-nil inside a transaction

	Device *DeviceRepo
	Backup *BackupRepo
}

// NewStore wires the repo graph on top of db.
func NewStore(db *sqlx.DB) *Store {
	s := &Store{db: db}
	s.wireRepos()
	return s
}

func (s *Store) wireRepos() {
	s.Device = &DeviceRepo{s: s}
	s.Backup = &BackupRepo{s: s}
}

// Ping reports DB reachability for the /healthz endpoint.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// ext is the in-flight transaction when there is one, the shared pool otherwise.
func (s *Store) ext() sqlx.ExtContext {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// WithTx runs fn inside one transaction; the Store it receives routes every repo
// to it, and nested calls join the outer one. Connections use _txlock=immediate,
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
