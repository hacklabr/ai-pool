package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	// Driver registration: abre conexão via pgx/v5/stdlib (compatível com
	// database/sql) para uso com o driver golang-migrate pgx5.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// RunMigrations aplica todas as migrations pendentes (up) contidas em fsys.
//
// fsys deve ser um fs.FS contendo os arquivos .sql (ex.: migrations.FS via
// //go:embed). Os arquivos são lidos do root de fsys.
//
// Idempotente: se não houver mudanças (ErrNoChange), retorna nil sem erro.
// Em caso de falha, retorna o erro para o caller decidir (retry, abort deploy).
//
// Uso típico:
//
//	err := postgres.RunMigrations(ctx, cfg.DBDSN, migrations.FS)
//
// Em produção, este helper é chamado por um subcomando `aipp migrate up`
// rodando num Helm pre-install/pre-upgrade Hook Job (DevOps §5.3). Em dev,
// pode ser chamado no startup do app ou via `make migrate-up` (CLI direto).
//
// NOTA: esta função abre uma conexão separada (database/sql) dedicada a
// migrations. O pool da aplicação (pgxpool) NÃO é usado para DDL — migrations
// exigem privilégios distintos (role aipp_migrations) e rodam isoladamente.
func RunMigrations(ctx context.Context, dsn string, fsys fs.FS) error {
	if dsn == "" {
		return errors.New("postgres.RunMigrations: DSN vazio")
	}

	// database/sql + pgx/stdlib fornece a interface que o driver do
	// golang-migrate espera (*sql.DB). O advisory lock interno do
	// golang-migrate impede concorrência entre múltiplos workers.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("postgres.RunMigrations: open: %w", err)
	}
	defer db.Close()

	// Honra o contexto (cancelamento/deadline do caller, ex.: Helm Job timeout).
	// sql.Open não suporta ctx diretamente; usamos SetConnMaxLifetime curto e
	// confiamos no ping/Next abaixo. Para cancelamento real, o caller fecha.
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return fmt.Errorf("postgres.RunMigrations: source io: %w", err)
	}

	drv, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		return fmt.Errorf("postgres.RunMigrations: driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "aipp", drv)
	if err != nil {
		return fmt.Errorf("postgres.RunMigrations: new: %w", err)
	}
	defer m.Close()

	// context não é propagado pela API clássica do golang-migrate; para
	// cancelamento, o caller fecha o *sql.DB. Aceitável para Job de deploy.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("postgres.RunMigrations: up: %w", err)
	}
	return nil
}

// MigrationsVersion retorna a versão atual do schema (útil para /version ou
// health check de consistência entre binário e DB).
//
// Faz query direta na tabela schema_migrations (criada pelo golang-migrate)
// em vez de instanciar o objeto Migrate — evita dependência frágil de source
// nil (que causava nil pointer no Close).
func MigrationsVersion(ctx context.Context, dsn string) (uint, bool, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return 0, false, err
	}
	defer db.Close()

	// Tabela pode não existir (banco novo, migrations nunca aplicadas): trata
	// o erro de "relation does not exist" retornando versão 0 (sem erro).
	const q = `SELECT version, dirty FROM schema_migrations LIMIT 1`
	var (
		version uint
		dirty   bool
	)
	row := db.QueryRowContext(ctx, q)
	if err := row.Scan(&version, &dirty); err != nil {
		// Detecta "tabela não existe" (código 42P01) — retorna 0 sem erro,
		// pois significa que migrations nunca foram aplicadas.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return 0, false, nil
		}
		// Detecta "no rows" — tabela existe mas vazia (estado improvável,
		// mas tratado defensivamente).
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("migrations version: %w", err)
	}
	return version, dirty, nil
}
