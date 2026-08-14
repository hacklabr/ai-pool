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

// insertUsageLog insere uma linha de usage_logs para o teste (FK lógica —
// key_id não precisa existir). Retorna erro do INSERT.
func insertUsageLog(ctx context.Context, pool *pgxpool.Pool, orgID, keyID int64, prompt, completion, cached int, createdAt time.Time) error {
	const q = `INSERT INTO usage_logs
		(organization_id, user_id, token_id, provider_id, key_id, model, prompt_tokens, completion_tokens, cached_tokens, status_code, created_at)
	VALUES ($1, 1, 1, 1, $2, 'glm-4.6', $3, $4, $5, 200, $6)`
	_, err := pool.Exec(ctx, q, orgID, keyID, prompt, completion, cached, createdAt)
	return err
}

// TestUsageLogRepo_QueryKeyUsageInWindow valida a soma por chave numa janela
// [from, to) (issue #20, RF-02): batching de várias chaves em UMA query,
// exclusão dos limites (>= from, < to) e isolamento por org. Requer
// AIPP_TEST_DB apontando para um PostgreSQL descartável.
func TestUsageLogRepo_QueryKeyUsageInWindow(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	repo := NewUsageLogRepo(pool)

	now := time.Now().UTC()
	// Chave 10: 2 logs DENTRO da janela (total 300) e 1 FORA (não conta).
	logs := []struct {
		key                int64
		prompt, completion int
		at                 time.Time
	}{
		{10, 100, 50, now.Add(-2 * time.Hour)},
		{10, 100, 50, now.Add(-1 * time.Hour)},
		{10, 999, 999, now.Add(-10 * 24 * time.Hour)}, // fora (antes de from)
		{11, 70, 30, now.Add(-3 * time.Hour)},         // outra chave no batch
		{11, 5, 5, now},                               // exatamente em `to` → FORA (< to)
	}
	for _, l := range logs {
		if err := insertUsageLog(ctx, pool, 1, l.key, l.prompt, l.completion, 0, l.at); err != nil {
			t.Fatalf("insert key %d: %v", l.key, err)
		}
	}
	// Log de OUTRA org para a mesma chave — não pode vazar.
	if err := insertUsageLog(ctx, pool, 2, 10, 500, 500, 0, now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert outra org: %v", err)
	}

	from := now.Add(-24 * time.Hour)
	got, err := repo.QueryKeyUsageInWindow(ctx, 1, []int64{10, 11}, from, now)
	if err != nil {
		t.Fatalf("QueryKeyUsageInWindow: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("chaves retornadas = %d, want 2 (chaves sem uso não contam)", len(got))
	}
	u10 := got[10]
	if u10.PromptTokens != 200 || u10.CompletionTokens != 100 || u10.TotalTokens != 300 {
		t.Errorf("key 10 = %+v, want prompt=200 completion=100 total=300", u10)
	}
	u11 := got[11]
	if u11.TotalTokens != 100 {
		t.Errorf("key 11 = %+v, want total=100 (log em `to` excluído)", u11)
	}

	// Batch vazio: sem query, mapa vazio.
	empty, err := repo.QueryKeyUsageInWindow(ctx, 1, nil, from, now)
	if err != nil || len(empty) != 0 {
		t.Errorf("batch vazio: got %v err=%v, want mapa vazio sem erro", empty, err)
	}
}

// TestKeyQuotaMeasurementRepo_MinTokensByLayer valida o piso conservador
// (MIN) por camada usado como denominador calibrado (issue #20, RF-05).
// Requer AIPP_TEST_DB.
func TestKeyQuotaMeasurementRepo_MinTokensByLayer(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	seed := []KeyQuotaMeasurement{
		{OrganizationID: 1, KeyID: 10, LimitLayer: "weekly", Tokens: 900_000},
		{OrganizationID: 1, KeyID: 10, LimitLayer: "weekly", Tokens: 800_000}, // MIN weekly = 800k
		{OrganizationID: 1, KeyID: 10, LimitLayer: "monthly", Tokens: 3_200_000},
		{OrganizationID: 1, KeyID: 11, LimitLayer: "monthly", Tokens: 2_500_000},
		{OrganizationID: 1, KeyID: 11, LimitLayer: "window_5h", Tokens: 7}, // fora do filtro
		{OrganizationID: 2, KeyID: 10, LimitLayer: "weekly", Tokens: 1},    // outra org
	}
	for i := range seed {
		if _, err := repo.Create(ctx, &seed[i]); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	got, err := repo.MinTokensByLayer(ctx, 1, []int64{10, 11})
	if err != nil {
		t.Fatalf("MinTokensByLayer: %v", err)
	}
	if got[10]["weekly"] != 800_000 {
		t.Errorf("key 10 weekly = %d, want 800000 (MIN)", got[10]["weekly"])
	}
	if got[10]["monthly"] != 3_200_000 {
		t.Errorf("key 10 monthly = %d, want 3200000", got[10]["monthly"])
	}
	if got[11]["monthly"] != 2_500_000 {
		t.Errorf("key 11 monthly = %d, want 2500000", got[11]["monthly"])
	}
	if _, ok := got[11]["window_5h"]; ok {
		t.Errorf("window_5h não deve retornar (não é denominador de ciclo)")
	}
	if _, ok := got[12]; ok {
		t.Errorf("chave sem medições não deve aparecer no mapa")
	}
}

// TestQuotaCalibrationAdapter_RecordCeilingHit valida o adapter da porta
// routing.QuotaMeasurementRecorder (issue #20, RF-05): janela de 5h fixa,
// soma correta dos tokens, skip intencional para chave sem data de renovação
// e camada que não é teto. Requer AIPP_TEST_DB.
func TestQuotaCalibrationAdapter_RecordCeilingHit(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	// Chave com data de renovação (âncora dia 10) e chave sem âncora.
	const seedKeys = `INSERT INTO organizations (name, slug) VALUES ('org', 'org1');
		INSERT INTO providers (organization_id, name, slug, base_url) VALUES (1, 'Z.ai', 'zai', 'https://api.z.ai');
		INSERT INTO provider_keys (provider_id, organization_id, label, encrypted_value, plan_renewal_date, plan_quota_tokens)
		VALUES (1, 1, 'com-plano', '\x00', DATE '2026-01-10', 1200000);
		INSERT INTO provider_keys (provider_id, organization_id, label, encrypted_value)
		VALUES (1, 1, 'sem-plano', '\x00');`
	if _, err := pool.Exec(ctx, seedKeys); err != nil {
		t.Fatalf("seed: %v", err)
	}

	now := time.Now().UTC()
	// Uso da chave com-plano dentro da janela de 5h: total 300.
	if err := insertUsageLog(ctx, pool, 1, 1, 100, 50, 0, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := insertUsageLog(ctx, pool, 1, 1, 100, 50, 0, now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Fora da janela de 5h (não conta para window_5h).
	if err := insertUsageLog(ctx, pool, 1, 1, 999, 999, 0, now.Add(-6*time.Hour)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	adapter := NewQuotaCalibrationAdapter(pool)

	// 1) window_5h: janela fixa [now-5h, now).
	msg := `{"code":"1308"} limit reached`
	if err := adapter.RecordCeilingHit(ctx, 1, 1, "window_5h", now, msg); err != nil {
		t.Fatalf("RecordCeilingHit window_5h: %v", err)
	}
	// 2) monthly na chave com âncora: janela ancorada no dia 10.
	if err := adapter.RecordCeilingHit(ctx, 1, 1, "monthly", now, msg); err != nil {
		t.Fatalf("RecordCeilingHit monthly: %v", err)
	}
	// 3) monthly na chave SEM âncora: skip intencional (nil, sem medição).
	if err := adapter.RecordCeilingHit(ctx, 1, 2, "monthly", now, msg); err != nil {
		t.Fatalf("RecordCeilingHit sem âncora: %v (skip deve ser nil)", err)
	}
	// 4) camada que não é teto: defesa em profundidade.
	if err := adapter.RecordCeilingHit(ctx, 1, 1, "balance", now, msg); err != nil {
		t.Fatalf("RecordCeilingHit balance: %v", err)
	}

	measRepo := NewKeyQuotaMeasurementRepo(pool)
	rows, err := measRepo.ListByKey(ctx, 1, 1, 0)
	if err != nil {
		t.Fatalf("ListByKey: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("medições = %d, want 2 (window_5h + monthly)", len(rows))
	}
	byLayer := map[string]KeyQuotaMeasurement{}
	for _, r := range rows {
		byLayer[r.LimitLayer] = r
	}
	w5h := byLayer["window_5h"]
	if w5h.Tokens != 300 {
		t.Errorf("window_5h tokens = %d, want 300 (soma da janela de 5h)", w5h.Tokens)
	}
	// TIMESTAMPTZ tem precisão de microssegundos — compara com tolerância.
	const tol = 10 * time.Microsecond
	if d := w5h.WindowEnd.Sub(now); d > tol || d < -tol {
		t.Errorf("window_5h WindowEnd = %v, want %v", w5h.WindowEnd, now)
	}
	if d := now.Sub(w5h.WindowStart) - 5*time.Hour; d > tol || d < -tol {
		t.Errorf("window_5h janela = [%v, %v), want [now-5h, now)", w5h.WindowStart, w5h.WindowEnd)
	}
	if w5h.ErrorMessage == nil || *w5h.ErrorMessage != msg {
		t.Errorf("window_5h error_message = %v, want %q", w5h.ErrorMessage, msg)
	}
	mon := byLayer["monthly"]
	// Ciclo mensal corrente ancorado no dia 10: começa no aniversário <= now.
	if mon.Tokens < 300 {
		t.Errorf("monthly tokens = %d, want >= 300 (janela mensal contém a de 5h)", mon.Tokens)
	}
	if mon.WindowStart.After(now) || mon.WindowEnd.Before(now) {
		t.Errorf("monthly janela [%v, %v) não contém now=%v", mon.WindowStart, mon.WindowEnd, now)
	}
}
