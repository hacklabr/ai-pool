package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageLogRecord é o registro bruto de uso de uma requisição (spec §3.4.1 item 8).
//
// É definido AQUI (não em domain/) porque é um detalhe de persistência: o
// domínio trata "uso" como evento efêmero; a persistência agrega/roteia para
// logs particionados + rollups + dead-letter. Manter este tipo fora do domínio
// evita acoplar o núcleo a detalhes de storage.
type UsageLogRecord struct {
	OrganizationID   int64
	UserID           int64
	TokenID          int64
	ProviderID       int64
	KeyID            int64
	Model            string
	RequestID        *string // UUID string; nil se não houver
	Stream           bool
	PromptTokens     *int
	CompletionTokens *int
	CachedTokens     int
	TotalTokens      *int
	LatencyMS        *int
	StatusCode       *int
	ErrorClass       *string
	ClientIP         *string
	ClientUA         *string
	CostUSD          *float64
}

// UsageLogRepo persiste logs de uso em usage_logs (particionada mensal) e
// mantém a fila de retrabalho (dead-letter) e os rollups agregados (spec §3.4).
//
// ATENÇÃO — durabilidade (spec §3.4.2):
//   - InsertBatch NÃO deve bloquear o path do request. O caller (worker de
//     background em F3) consome de um canal e chama InsertBatch em batches.
//   - Se InsertBatch falha (partição ausente, DB indisponível), o caller deve
//     chamar InsertDeadLetter com o payload original, NÃO descartar silenciosamente.
//   - WAL é a garantia real de durabilidade: após COMMIT do batch, os logs
//     sobrevivem a crash do Postgres. O que está no canal (em memória) perde-se
//     em crash do pod — janela aceitável (≤ batch_size × flush_interval).
type UsageLogRepo struct {
	pool *pgxpool.Pool
}

// NewUsageLogRepo cria o repositório.
func NewUsageLogRepo(pool *pgxpool.Pool) *UsageLogRepo { return &UsageLogRepo{pool: pool} }

// InsertBatch insere múltiplos registros em UMA round-trip (multi-row VALUES).
// Usa uma transação para atomicidade do batch (tudo ou nada).
//
// Em caso de erro, retorna o erro para o caller decidir (dead-letter).
// Não há UPSERT aqui — logs são append-only (id gerado).
func (r *UsageLogRepo) InsertBatch(ctx context.Context, records []UsageLogRecord) error {
	if len(records) == 0 {
		return nil
	}
	// Constrói o batch multi-row. Usando pgx.Batch para reduzir overhead de
	// prepared statements com cardinalidade variável.
	//
	// NOTA: Para volumes maiores, avaliar COPY (pgx.CopyFrom) que é ainda mais
	// rápido. Para o MVP (30 usuários, batches de ~50), multi-row INSERT é
	// suficiente e mais simples de rolar back.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("UsageLogRepo.InsertBatch begin: %w", err)
	}
	defer tx.Rollback(ctx) // safe: no-op se já committed

	const q = `
		INSERT INTO usage_logs (
			organization_id, user_id, token_id, provider_id, key_id, model,
			request_id, stream, prompt_tokens, completion_tokens, cached_tokens,
			total_tokens, latency_ms, status_code, error_class, client_ip, client_ua, cost_usd
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17, $18
		)`

	batch := &pgx.Batch{}
	for _, rec := range records {
		batch.Queue(q,
			rec.OrganizationID, rec.UserID, rec.TokenID, rec.ProviderID, rec.KeyID, rec.Model,
			parseUUID(rec.RequestID), rec.Stream, rec.PromptTokens, rec.CompletionTokens,
			rec.CachedTokens, rec.TotalTokens, rec.LatencyMS, rec.StatusCode,
			rec.ErrorClass, rec.ClientIP, rec.ClientUA, rec.CostUSD,
		)
	}
	res := tx.SendBatch(ctx, batch)
	// Consome resultados de cada queued statement para detectar erros.
	for range records {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return fmt.Errorf("UsageLogRepo.InsertBatch exec: %w", err)
		}
	}
	if err := res.Close(); err != nil {
		return fmt.Errorf("UsageLogRepo.InsertBatch close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("UsageLogRepo.InsertBatch commit: %w", err)
	}
	return nil
}

// InsertDeadLetter persiste um payload que falhou ao inserir em usage_logs.
// Permite reprocessamento posterior via job de reconciliação (spec §3.4.2).
// NUNCA falha silenciosamente — se esta inserção também falhar, o caller deve
// logar (stderr) e aceitar a perda (situação extrema de DB indisponível).
func (r *UsageLogRepo) InsertDeadLetter(ctx context.Context, payload any, errMsg string) error {
	const q = `INSERT INTO usage_logs_dead_letter (payload, error) VALUES ($1, $2)`
	_, err := r.pool.Exec(ctx, q, payload, errMsg)
	if err != nil {
		return fmt.Errorf("UsageLogRepo.InsertDeadLetter: %w", err)
	}
	return nil
}

// RollupRow é uma linha agregada por (dimensões, bucket) — usado para popular
// usage_rollups a partir de usage_logs, e para o dashboard ler rollups.
type RollupRow struct {
	OrganizationID   int64
	UserID           int64
	ProviderID       int64
	Model            string
	BucketHour       pgtype.Timestamptz
	RequestCount     int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	ErrorCount       int64
	TotalLatencyMS   int64
}

// AggregateHourFromLogs agrega uma janela fechada de usage_logs em uma lista
// de RollupRow (para upsert em usage_rollups).
//
// TIPICAMENTE chamado por um job (pg_cron ou aplicação) a cada hora, agregando
// a última hora fechada. Em seguida, UpsertRollups persiste o resultado.
func (r *UsageLogRepo) AggregateHourFromLogs(ctx context.Context, orgID int64, from, to pgtype.Timestamptz) ([]RollupRow, error) {
	const q = `
		SELECT
			organization_id, user_id, provider_id, model,
			date_trunc('hour', created_at) AS bucket_hour,
			COUNT(*)                                AS request_count,
			COALESCE(SUM(prompt_tokens), 0)         AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0)     AS completion_tokens,
			COALESCE(SUM(cached_tokens), 0)         AS cached_tokens,
			COALESCE(SUM(total_tokens), 0)          AS total_tokens,
			COUNT(*) FILTER (WHERE status_code >= 400) AS error_count,
			COALESCE(SUM(latency_ms), 0)            AS total_latency_ms
		FROM usage_logs
		WHERE organization_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY organization_id, user_id, provider_id, model, bucket_hour`
	rows, err := r.pool.Query(ctx, q, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("UsageLogRepo.AggregateHourFromLogs: %w", err)
	}
	defer rows.Close()

	var out []RollupRow
	for rows.Next() {
		var row RollupRow
		if err := rows.Scan(
			&row.OrganizationID, &row.UserID, &row.ProviderID, &row.Model,
			&row.BucketHour, &row.RequestCount, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &row.TotalTokens,
			&row.ErrorCount, &row.TotalLatencyMS,
		); err != nil {
			return nil, fmt.Errorf("UsageLogRepo.AggregateHourFromLogs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpsertRollups persiste agregados em usage_rollups com upsert incremental
// (ON CONFLICT DO UPDATE) — idempotente: re-agregar a mesma hora soma corretamente.
//
// A PK composta (organization_id, user_id, provider_id, model, bucket_hour)
// garante unicidade por bucket horário.
func (r *UsageLogRepo) UpsertRollups(ctx context.Context, rows []RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
		INSERT INTO usage_rollups (
			organization_id, user_id, provider_id, model, bucket_hour,
			request_count, prompt_tokens, completion_tokens, cached_tokens,
			total_tokens, error_count, total_latency_ms
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (organization_id, user_id, provider_id, model, bucket_hour)
		DO UPDATE SET
			request_count     = usage_rollups.request_count     + EXCLUDED.request_count,
			prompt_tokens     = usage_rollups.prompt_tokens     + EXCLUDED.prompt_tokens,
			completion_tokens = usage_rollups.completion_tokens + EXCLUDED.completion_tokens,
			cached_tokens     = usage_rollups.cached_tokens     + EXCLUDED.cached_tokens,
			total_tokens      = usage_rollups.total_tokens      + EXCLUDED.total_tokens,
			error_count       = usage_rollups.error_count       + EXCLUDED.error_count,
			total_latency_ms  = usage_rollups.total_latency_ms  + EXCLUDED.total_latency_ms,
			updated_at        = now()`
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("UsageLogRepo.UpsertRollups begin: %w", err)
	}
	defer tx.Rollback(ctx)

	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(q,
			row.OrganizationID, row.UserID, row.ProviderID, row.Model, row.BucketHour,
			row.RequestCount, row.PromptTokens, row.CompletionTokens, row.CachedTokens,
			row.TotalTokens, row.ErrorCount, row.TotalLatencyMS,
		)
	}
	res := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return fmt.Errorf("UsageLogRepo.UpsertRollups exec: %w", err)
		}
	}
	if err := res.Close(); err != nil {
		return fmt.Errorf("UsageLogRepo.UpsertRollups close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("UsageLogRepo.UpsertRollups commit: %w", err)
	}
	return nil
}

// parseUUID converte opcional *string UUID para pgtype.UUID. Retorna invalid
// (NULL) se nil ou vazio. Não valida formato rigorosamente — o Postgres valida.
func parseUUID(s *string) pgtype.UUID {
	if s == nil || *s == "" {
		return pgtype.UUID{Valid: false}
	}
	var u pgtype.UUID
	// Scan aceita string hexadecimal com hífens; em erro, deixa invalid.
	if err := u.Scan(*s); err != nil {
		return pgtype.UUID{Valid: false}
	}
	return u
}

// =============================================================================
// Leitura de rollups para o Dashboard (F8).
//
// O dashboard NUNCA agrega usage_logs em runtime (isto seria caro — logs crus
// vivem em tabela particionada de alto volume). Em vez disso, consome SOMENTE
// usage_rollups, que já é agregação horária mantida pelo RollupJob (spec §3.4.1
// item 9, Backend §3.4 T3). Consultas ficam sub-ms mesmo com anos de histórico.
//
// Exceção documentada (Backend T3): a "tabela das últimas N requisições"
// (live tail do dashboard) consulta usage_logs da partição corrente com LIMIT
// e prune — sub-ms. Não implementado neste repo (fica como handler separado se
// necessário no MVP).
// =============================================================================

// DashboardRollupRow é uma linha agregada por dimensão (usuário, provedor ou
// modelo) em uma janela de tempo. "GroupKey" é o rótulo legível (username,
// slug do provedor, ou nome do modelo).
type DashboardRollupRow struct {
	GroupKey         string // label legível (username, provider.slug, model)
	RequestCount     int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	ErrorCount       int64
}

// DashboardTotals agrega totais do período (somatório cru de todos os rollups).
// cached_tokens é o KPI destacado "tokens economizados por cache de contexto".
// NonCachedTokens = total − cached (tokens NÃO servidos do cache — o que de
// fato "custa" mais, já que cached é subset de prompt).
type DashboardTotals struct {
	RequestCount     int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	NonCachedTokens  int64
	ErrorCount       int64
}

// QueryRollupsForDashboard retorna agregações por dimensão + totais do período.
//
// groupBy ∈ {"user", "provider", "model"} (default: "user"). A query faz JOIN
// com a tabela correspondente (users/providers) para resolver o label legível.
// Para modelos, r.model é usado diretamente (sem JOIN — models pode ter linhas
// órfãs se o modelo foi removido; isto é desejado: o rollup preserva histórico).
//
// Período: [from, to) — half-open (consistente com AggregateHourFromLogs).
// Se from for zero-value, defaults para "há 7 dias"; se to for zero, agora.
//
// Retorna erro apenas em falha de infra (DB indisponível, scan). Lista vazia é
// resultado válido (sem uso no período).
func (r *UsageLogRepo) QueryRollupsForDashboard(ctx context.Context, orgID int64, from, to time.Time, groupBy string) ([]DashboardRollupRow, DashboardTotals, error) {
	// Defaults defensivos: dashboard nunca quebra por data zero.
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}

	// Normaliza groupBy (default user). Valores inválidos caem em "user".
	switch groupBy {
	case "user", "provider", "model":
		// ok
	default:
		groupBy = "user"
	}

	rows, err := r.pool.Query(ctx, dashboardRollupsQuery(groupBy), orgID, from, to)
	if err != nil {
		return nil, DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryRollupsForDashboard query: %w", err)
	}
	defer rows.Close()

	out := make([]DashboardRollupRow, 0)
	for rows.Next() {
		var row DashboardRollupRow
		if err := rows.Scan(
			&row.GroupKey, &row.RequestCount, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &row.TotalTokens,
			&row.ErrorCount,
		); err != nil {
			return nil, DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryRollupsForDashboard scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryRollupsForDashboard rows.Err: %w", err)
	}

	totals, err := r.QueryRollupsTotals(ctx, orgID, from, to)
	if err != nil {
		return nil, DashboardTotals{}, err
	}
	return out, totals, nil
}

// dashboardRollupsQuery monta a query SQL conforme a dimensão.
// A consulta é sempre sobre usage_rollups (agregação horária mantida pelo job).
//
// Notas de performance:
//   - Índices existentes (idx_rollups_provider_hour, idx_rollups_model_hour,
//     idx_rollups_hour) cobrem todos os modos via (organization_id, bucket_hour).
//   - LEFT JOIN preserva rollups cujo usuário/provedor foi deletado (rótulo vira
//     "(deleted)" — desejado para auditoria histórica).
//   - ORDER BY total_tokens DESC garante que os maiores consumidores aparecem
//     primeiro no dashboard (UX: ranking natural).
func dashboardRollupsQuery(groupBy string) string {
	// Base SELECT compartilhada. COUNT e SUMs são idênticos em todos os modos.
	const baseSelect = `SELECT
		%[1]s AS group_key,
		COALESCE(SUM(r.request_count), 0)     AS request_count,
		COALESCE(SUM(r.prompt_tokens), 0)     AS prompt_tokens,
		COALESCE(SUM(r.completion_tokens), 0) AS completion_tokens,
		COALESCE(SUM(r.cached_tokens), 0)     AS cached_tokens,
		COALESCE(SUM(r.total_tokens), 0)      AS total_tokens,
		COALESCE(SUM(r.error_count), 0)       AS error_count
	FROM usage_rollups r
	%[2]s
	WHERE r.organization_id = $1 AND r.bucket_hour >= $2 AND r.bucket_hour < $3
	GROUP BY %[3]s
	ORDER BY total_tokens DESC NULLS LAST`

	switch groupBy {
	case "provider":
		// LEFT JOIN com providers para resolver slug (fallback "(deleted)").
		return fmt.Sprintf(baseSelect,
			`COALESCE(p.slug, '(deleted)')`,
			`LEFT JOIN providers p ON p.id = r.provider_id`,
			`p.slug`)
	case "model":
		// Sem JOIN — models pode ter entries órfãs (model removido).
		// GROUP BY direto em r.model.
		return fmt.Sprintf(baseSelect, `r.model`, ``, `r.model`)
	default: // "user"
		// LEFT JOIN com users para resolver username (fallback "(deleted)").
		return fmt.Sprintf(baseSelect,
			`COALESCE(u.username, '(deleted)')`,
			`LEFT JOIN users u ON u.id = r.user_id`,
			`u.username`)
	}
}

// QueryRollupsTotals retorna o somatório total do período (todos os rollups,
// sem GROUP BY). Usado para KPI cards (cached_tokens é o KPI destacado).
func (r *UsageLogRepo) QueryRollupsTotals(ctx context.Context, orgID int64, from, to time.Time) (DashboardTotals, error) {
	const q = `SELECT
		COALESCE(SUM(request_count), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(total_tokens), 0) - COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(error_count), 0)
	FROM usage_rollups
	WHERE organization_id = $1 AND bucket_hour >= $2 AND bucket_hour < $3`
	var t DashboardTotals
	if err := r.pool.QueryRow(ctx, q, orgID, from, to).Scan(
		&t.RequestCount, &t.PromptTokens, &t.CompletionTokens,
		&t.CachedTokens, &t.TotalTokens, &t.NonCachedTokens, &t.ErrorCount,
	); err != nil {
		return DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryRollupsTotals: %w", err)
	}
	return t, nil
}

// QueryUserUsageTotals retorna o consumo agregado de UM usuário no período.
// Usado pela tela "Meu uso" (/web/me/usage) — escopado pelo user_id da sessão.
//
// Como usage_rollups tem user_id NOT NULL e PK começa por (organization_id,
// user_id, ...), a query é indexada e sub-ms.
func (r *UsageLogRepo) QueryUserUsageTotals(ctx context.Context, orgID, userID int64, from, to time.Time) (DashboardTotals, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT
		COALESCE(SUM(request_count), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(total_tokens), 0) - COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(error_count), 0)
	FROM usage_rollups
	WHERE organization_id = $1 AND user_id = $2 AND bucket_hour >= $3 AND bucket_hour < $4`
	var t DashboardTotals
	if err := r.pool.QueryRow(ctx, q, orgID, userID, from, to).Scan(
		&t.RequestCount, &t.PromptTokens, &t.CompletionTokens,
		&t.CachedTokens, &t.TotalTokens, &t.NonCachedTokens, &t.ErrorCount,
	); err != nil {
		return DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryUserUsageTotals: %w", err)
	}
	return t, nil
}

// QueryUserUsageByModel retorna breakdown por modelo para um usuário.
// Usado pela tela "Meu uso" (gráfico donut de distribuição por modelo).
func (r *UsageLogRepo) QueryUserUsageByModel(ctx context.Context, orgID, userID int64, from, to time.Time) ([]DashboardRollupRow, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT
		model AS group_key,
		COALESCE(SUM(request_count), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(error_count), 0)
	FROM usage_rollups
	WHERE organization_id = $1 AND user_id = $2 AND bucket_hour >= $3 AND bucket_hour < $4
	GROUP BY model
	ORDER BY SUM(total_tokens) DESC NULLS LAST`
	rows, err := r.pool.Query(ctx, q, orgID, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("UsageLogRepo.QueryUserUsageByModel: %w", err)
	}
	defer rows.Close()
	out := make([]DashboardRollupRow, 0)
	for rows.Next() {
		var row DashboardRollupRow
		if err := rows.Scan(
			&row.GroupKey, &row.RequestCount, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &row.TotalTokens,
			&row.ErrorCount,
		); err != nil {
			return nil, fmt.Errorf("UsageLogRepo.QueryUserUsageByModel scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// =============================================================================
// Queries em TEMPO REAL (lê usage_logs diretamente — não rollups).
//
// Usadas pela tela "Meu uso" com auto-atualização por polling (10s). Ler logs
// direto dá frescor de ~10s (cada poll reflete as últimas requisições), ao
// invés dos rollups que lagam até 1 min (job periódico).
//
// Performance: cobertas pelo índice idx_usage_logs_user_time
// (organization_id, user_id, created_at DESC) — janela de 7d por usuário é
// rápida mesmo com volume. usage_logs NÃO tem coluna total_tokens/request_count:
// total_tokens = prompt+completion; request_count = COUNT(*).
// =============================================================================

// QueryUserUsageTotalsFromLogs é a versão em tempo real de QueryUserUsageTotals.
// Mesmo shape de saída (DashboardTotals), mas lê usage_logs.
func (r *UsageLogRepo) QueryUserUsageTotalsFromLogs(ctx context.Context, orgID, userID int64, from, to time.Time) (DashboardTotals, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0) - COALESCE(SUM(cached_tokens), 0),
		COUNT(*) FILTER (WHERE status_code >= 400)
	FROM usage_logs
	WHERE organization_id = $1 AND user_id = $2 AND created_at >= $3 AND created_at < $4`
	var t DashboardTotals
	if err := r.pool.QueryRow(ctx, q, orgID, userID, from, to).Scan(
		&t.RequestCount, &t.PromptTokens, &t.CompletionTokens,
		&t.CachedTokens, &t.TotalTokens, &t.NonCachedTokens, &t.ErrorCount,
	); err != nil {
		return DashboardTotals{}, fmt.Errorf("UsageLogRepo.QueryUserUsageTotalsFromLogs: %w", err)
	}
	return t, nil
}

// QueryUserUsageByModelFromLogs é a versão em tempo real de QueryUserUsageByModel.
// Retorna breakdown por modelo (para o donut) lendo usage_logs.
func (r *UsageLogRepo) QueryUserUsageByModelFromLogs(ctx context.Context, orgID, userID int64, from, to time.Time) ([]DashboardRollupRow, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT
		COALESCE(NULLIF(model, ''), '(unknown)') AS group_key,
		COUNT(*) AS request_count,
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0),
		COUNT(*) FILTER (WHERE status_code >= 400)
	FROM usage_logs
	WHERE organization_id = $1 AND user_id = $2 AND created_at >= $3 AND created_at < $4
	GROUP BY model
	ORDER BY (SUM(prompt_tokens) + SUM(completion_tokens)) DESC NULLS LAST`
	rows, err := r.pool.Query(ctx, q, orgID, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("UsageLogRepo.QueryUserUsageByModelFromLogs: %w", err)
	}
	defer rows.Close()
	out := make([]DashboardRollupRow, 0)
	for rows.Next() {
		var row DashboardRollupRow
		if err := rows.Scan(
			&row.GroupKey, &row.RequestCount, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &row.TotalTokens,
			&row.ErrorCount,
		); err != nil {
			return nil, fmt.Errorf("UsageLogRepo.QueryUserUsageByModelFromLogs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// =============================================================================
// Quebra por CHAVE e série HORÁRIA (logs, tempo real).
//
// Usadas pela página de detalhe do usuário e pela visão de chaves. userID=0
// significa "todos os usuários" (global) via ($N = 0 OR user_id = $N).
// =============================================================================

// KeyUsageRow é uma linha de uso agregada por chave de provedor.
type KeyUsageRow struct {
	KeyID            int64
	ProviderID       int64
	Label            string
	RequestCount     int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	NonCachedTokens  int64
	ErrorCount       int64
}

// QueryKeyUsageFromLogs agrega uso por key_id. userID=0 → global (todos os
// usuários); >0 → só esse usuário. LEFT JOIN provider_keys pega o label (chaves
// removidas viram "(deleted)").
func (r *UsageLogRepo) QueryKeyUsageFromLogs(ctx context.Context, orgID, userID int64, from, to time.Time) ([]KeyUsageRow, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT l.key_id, l.provider_id, COALESCE(k.label,'(deleted)'),
		COUNT(*),
		COALESCE(SUM(l.prompt_tokens),0),
		COALESCE(SUM(l.completion_tokens),0),
		COALESCE(SUM(l.cached_tokens),0),
		COALESCE(SUM(l.prompt_tokens),0)+COALESCE(SUM(l.completion_tokens),0),
		COALESCE(SUM(l.prompt_tokens),0)+COALESCE(SUM(l.completion_tokens),0)-COALESCE(SUM(l.cached_tokens),0),
		COUNT(*) FILTER (WHERE l.status_code >= 400)
	FROM usage_logs l
	LEFT JOIN provider_keys k ON k.id = l.key_id
	WHERE l.organization_id = $1 AND ($2 = 0 OR l.user_id = $2)
	  AND l.created_at >= $3 AND l.created_at < $4
	GROUP BY l.key_id, l.provider_id, k.label
	ORDER BY (SUM(l.prompt_tokens)+SUM(l.completion_tokens)) DESC NULLS LAST`
	rows, err := r.pool.Query(ctx, q, orgID, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("UsageLogRepo.QueryKeyUsageFromLogs: %w", err)
	}
	defer rows.Close()
	out := make([]KeyUsageRow, 0)
	for rows.Next() {
		var row KeyUsageRow
		if err := rows.Scan(
			&row.KeyID, &row.ProviderID, &row.Label,
			&row.RequestCount, &row.PromptTokens, &row.CompletionTokens,
			&row.CachedTokens, &row.TotalTokens, &row.NonCachedTokens, &row.ErrorCount,
		); err != nil {
			return nil, fmt.Errorf("UsageLogRepo.QueryKeyUsageFromLogs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// HourlyRow é um ponto da série horária (para o gráfico de utilização por hora).
type HourlyRow struct {
	BucketHour   time.Time
	RequestCount int64
	TotalTokens  int64
	CachedTokens int64
}

// QueryUserHourlyFromLogs retorna a série horária de uso de um usuário.
// Janela típica: últimas 24h ou 7d. Agrupa por date_trunc('hour', created_at).
func (r *UsageLogRepo) QueryUserHourlyFromLogs(ctx context.Context, orgID, userID int64, from, to time.Time) ([]HourlyRow, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -7)
	}
	if to.IsZero() {
		to = time.Now()
	}
	const q = `SELECT date_trunc('hour', created_at) AS bucket_hour,
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0)+COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cached_tokens),0)
	FROM usage_logs
	WHERE organization_id = $1 AND user_id = $2 AND created_at >= $3 AND created_at < $4
	GROUP BY bucket_hour
	ORDER BY bucket_hour`
	rows, err := r.pool.Query(ctx, q, orgID, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("UsageLogRepo.QueryUserHourlyFromLogs: %w", err)
	}
	defer rows.Close()
	out := make([]HourlyRow, 0)
	for rows.Next() {
		var row HourlyRow
		if err := rows.Scan(&row.BucketHour, &row.RequestCount, &row.TotalTokens, &row.CachedTokens); err != nil {
			return nil, fmt.Errorf("UsageLogRepo.QueryUserHourlyFromLogs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// =============================================================================
// Stubs para fases futuras:
//
//   - DropOldPartitions(before)                           -> retention 90d (F6).
//   - EnsureFuturePartitions(monthsAhead)                 -> pré-criação (F6).
//     CRÍTICO: sem partição do mês corrente/próximo, TODOS os InsertBatch
//     falham (spec §3.4.2). Job periódico (CronJob K8s ou aplicação) cria.
//   - ReprocessDeadLetter()                               -> reconciliação (F6).
// =============================================================================
