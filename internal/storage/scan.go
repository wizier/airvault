package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wizier/airvault/internal/domain"

	"github.com/jmoiron/sqlx"
)

func getOne[T any](ctx context.Context, db sqlx.QueryerContext, query string, args ...any) (*T, error) {
	var v T
	if err := sqlx.GetContext(ctx, db, &v, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

func listOf[T any](ctx context.Context, db sqlx.QueryerContext, query string, args ...any) ([]T, error) {
	var rows []T
	if err := sqlx.SelectContext(ctx, db, &rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

// updatedOne is an UPDATE's outcome: ErrNotFound when it matched no row.
func updatedOne(result sql.Result, err error, op string) error {
	if err != nil {
		return wrap(err, op)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return wrap(err, op)
	}
	if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func wrap(err error, op string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}
