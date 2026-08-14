//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestKeyQuotaMeasurementRepo_CreateAndListByKey valida o repositório de
// medições de cota (issue #19, RF-05): criação + listagem por chave com as
// colunas de plano/janela. Segue o padrão dos testes de integração: requer
// AIPP_TEST_DB apontando para um PostgreSQL descartável.
func TestKeyQuotaMeasurementRepo_CreateAndListByKey(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Limpa schema e roda todas as migrations (inclui a da issue #19).
	if err := execRaw(ctx, dsn, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("setup drop: %v", err)
	}
	if err := RunMigrations(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	repo := NewKeyQuotaMeasurementRepo(pool)

	msg := `{"error":{"code":"1308"}} limit reached`
	m1 := &KeyQuotaMeasurement{
		OrganizationID: 1,
		KeyID:          42,
		LimitLayer:     "window_5h",
		Tokens:         118000,
		WindowStart:    time.Now().Add(-5 * time.Hour).UTC(),
		WindowEnd:      time.Now().UTC(),
		ErrorMessage:   &msg,
	}
	id1, err := repo.Create(ctx, m1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("id = %d, want > 0", id1)
	}

	// Segunda medição de outra camada (mais antiga — para validar ORDER BY).
	m2 := &KeyQuotaMeasurement{
		OrganizationID: 1,
		KeyID:          42,
		LimitLayer:     "weekly",
		Tokens:         900000,
		WindowStart:    time.Now().Add(-8 * 24 * time.Hour).UTC(),
		WindowEnd:      time.Now().Add(-7 * 24 * time.Hour).UTC(),
	}
	if _, err := repo.Create(ctx, m2); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	rows, err := repo.ListByKey(ctx, 1, 42, 0)
	if err != nil {
		t.Fatalf("ListByKey: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Mais recentes primeiro — mas os dois INSERTs têm created_at ~ iguais,
	// então validamos por camada (não por posição).
	var gotErrMsg *string
	layers := map[string]bool{}
	for _, r := range rows {
		layers[r.LimitLayer] = true
		if r.KeyID != 42 || r.Tokens <= 0 {
			t.Errorf("linha inesperada: %+v", r)
		}
		if r.LimitLayer == "window_5h" {
			gotErrMsg = r.ErrorMessage
		}
	}
	if !layers["window_5h"] || !layers["weekly"] {
		t.Fatalf("camadas = %v, want window_5h+weekly", layers)
	}
	if gotErrMsg == nil {
		t.Errorf("error_message deveria estar preenchido na medição window_5h")
	}

	// Outra org não vê as medições (multi-tenant).
	rows, err = repo.ListByKey(ctx, 2, 42, 0)
	if err != nil {
		t.Fatalf("ListByKey outra org: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows outra org = %d, want 0", len(rows))
	}
}
