//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql para o pgx
)

// Helpers de teste (apenas build integration) que abrem uma conexão database/sql
// descartável para setup/teardown e asserções. Não fazem parte da API pública.

func openTestDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open test db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping test db: %w", err)
	}
	return db, nil
}

func execRaw(ctx context.Context, dsn, stmt string) error {
	db, err := openTestDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, stmt)
	return err
}

// queryRowScan executa q (com args) e scaneia o resultado em dest ponteiro.
// Suporta *bool e *string (suficiente para os asserts do teste de integração).
func queryRowScan(ctx context.Context, dsn, q string, dest any, args ...any) error {
	db, err := openTestDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	row := db.QueryRowContext(ctx, q, args...)
	switch d := dest.(type) {
	case *bool:
		return row.Scan(d)
	case *string:
		return row.Scan(d)
	default:
		return fmt.Errorf("unsupported dest type %T", dest)
	}
}
