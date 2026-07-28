package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wizier/airvault/internal/domain"

	"github.com/jmoiron/sqlx"
)

// getOne wraps sqlx.GetContext, converting sql.ErrNoRows to domain.ErrNotFound.
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

// listOf runs a multi-row SELECT into a slice.
func listOf[T any](ctx context.Context, db sqlx.QueryerContext, query string, args ...any) ([]T, error) {
	var rows []T
	if err := sqlx.SelectContext(ctx, db, &rows, query, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

// wrap annotates a repo error with the operation. ErrNoRows never reaches it
// (Exec results don't produce it; getOne handles the query case itself).
func wrap(err error, op string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}
