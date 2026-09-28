package storage

import (
	"context"

	"github.com/jmoiron/sqlx"
)

type Store struct {
	db *sqlx.DB
	tx *sqlx.Tx

	Device *DeviceRepo
	Backup *BackupRepo
}

func NewStore(db *sqlx.DB) *Store {
	s := &Store{db: db}
	s.wireRepos()
	return s
}

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
