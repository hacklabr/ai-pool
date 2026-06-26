//go:build integration

// Teste de integração do helper de migrations. Roda apenas com a build tag
// `integration` e requer AIPP_TEST_DB apontando para um PostgreSQL alcançável
// e descartável (NUNCA apontar para produção).
//
//	Uso:
//	  export AIPP_TEST_DB="postgres://test:test@localhost:5434/aipp?sslmode=disable"
//	  go test -tags integration ./internal/adapters/postgres/
//
// Valida que:
//  1. O embed.FS (migrations.FS) é legível pelo source/iofs.
//  2. O driver database/pgx/v5 aplica as migrations.
//  3. MigrationsVersion retorna a versão esperada.
//  4. Reaplicar é idempotente (ErrNoChange tratado como nil).
//  5. O schema resultante tem as tabelas esperadas.
package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rafael/ai-pool/migrations"
)

func TestRunMigrations_AppliesSchema(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração de migrations")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Garante estado limpo (caso um teste anterior tenha falhado no meio).
	dropAll := `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`
	if err := execRaw(ctx, dsn, dropAll); err != nil {
		t.Fatalf("setup drop: %v", err)
	}

	// 1. Aplica via helper (embed.FS + pgx/v5 driver).
	if err := RunMigrations(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("RunMigrations up: %v", err)
	}

	// 2. Versão esperada = timestamp da primeira migration.
	v, dirty, err := MigrationsVersion(ctx, dsn)
	if err != nil {
		t.Fatalf("MigrationsVersion: %v", err)
	}
	if dirty {
		t.Fatalf("schema marcado como dirty")
	}
	if v != 202606252100 {
		t.Fatalf("versão esperada 202606252100, obtida %d", v)
	}

	// 3. Idempotência: reaplicar deve ser no-op (ErrNoChange tratado como nil).
	if err := RunMigrations(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("RunMigrations idempotente: %v", err)
	}

	// 4. Valida presença das tabelas críticas do MVP.
	wantTables := []string{
		"organizations", "users", "api_tokens", "providers", "provider_keys",
		"models", "rate_limits", "usage_logs", "usage_rollups", "audit_log",
		"usage_logs_dead_letter",
	}
	for _, tbl := range wantTables {
		var exists bool
		q := `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)`
		if err := queryRowScan(ctx, dsn, q, &exists, tbl); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("tabela esperada ausente após migration: %s", tbl)
		}
	}

	// 5. Valida seed da organização default (id=1).
	var orgName string
	if err := queryRowScan(ctx, dsn, `SELECT name FROM organizations WHERE id=1`, &orgName); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if orgName != "Default Organization" {
		t.Errorf("seed org name = %q, queria 'Default Organization'", orgName)
	}
}
