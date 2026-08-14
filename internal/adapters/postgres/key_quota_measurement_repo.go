package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// KeyQuotaMeasurementRepo — medições observadas de cota por chave (issue #19,
// RF-05). A cada desativação por teto, a soma de tokens da janela é persistida
// como uma medição da cota real (piso conservador), refinando o denominador
// configurado (provider_keys.plan_quota_tokens) ao longo dos ciclos.
//
// Sem FK física para provider_keys — segue a convenção de FK LÓGICA do schema
// (como usage_logs), permitindo referenciar entidades deletadas no histórico.
// =============================================================================

// KeyQuotaMeasurement é uma medição observada de cota (linha de
// key_quota_measurements).
type KeyQuotaMeasurement struct {
	ID             int64
	OrganizationID int64
	KeyID          int64
	LimitLayer     string // 'window_5h'|'weekly'|'monthly'|'balance'|'tier'|'other'
	Tokens         int64  // soma observada na janela
	WindowStart    time.Time
	WindowEnd      time.Time
	ErrorMessage   *string // mensagem upstream sanitizada (≤200 chars, sem segredo)
	CreatedAt      time.Time
}

// KeyQuotaMeasurementRepo persiste e lê medições de cota (append-orientado).
type KeyQuotaMeasurementRepo struct {
	pool *pgxpool.Pool
}

// NewKeyQuotaMeasurementRepo cria o repositório.
func NewKeyQuotaMeasurementRepo(pool *pgxpool.Pool) *KeyQuotaMeasurementRepo {
	return &KeyQuotaMeasurementRepo{pool: pool}
}

// Create insere uma medição observada de cota.
func (r *KeyQuotaMeasurementRepo) Create(ctx context.Context, m *KeyQuotaMeasurement) (int64, error) {
	if m == nil {
		return 0, fmt.Errorf("KeyQuotaMeasurementRepo.Create: nil measurement")
	}
	const q = `
		INSERT INTO key_quota_measurements
			(organization_id, key_id, limit_layer, tokens, window_start, window_end, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		m.OrganizationID, m.KeyID, m.LimitLayer, m.Tokens,
		m.WindowStart, m.WindowEnd, ptrToText(m.ErrorMessage),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("KeyQuotaMeasurementRepo.Create: %w", err)
	}
	return id, nil
}

// ListByKey retorna as medições de uma chave, mais recentes primeiro.
// limit é clampado em [1, 200]; 0 usa default 50.
func (r *KeyQuotaMeasurementRepo) ListByKey(ctx context.Context, orgID int64, keyID int64, limit int) ([]KeyQuotaMeasurement, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	const q = `
		SELECT id, organization_id, key_id, limit_layer, tokens,
		       window_start, window_end, error_message, created_at
		FROM key_quota_measurements
		WHERE organization_id = $1 AND key_id = $2
		ORDER BY created_at DESC
		LIMIT $3`
	rows, err := r.pool.Query(ctx, q, orgID, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("KeyQuotaMeasurementRepo.ListByKey: %w", err)
	}
	defer rows.Close()

	out := make([]KeyQuotaMeasurement, 0, limit)
	for rows.Next() {
		var m KeyQuotaMeasurement
		if err := rows.Scan(
			&m.ID, &m.OrganizationID, &m.KeyID, &m.LimitLayer, &m.Tokens,
			&m.WindowStart, &m.WindowEnd, &m.ErrorMessage, &m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("KeyQuotaMeasurementRepo.ListByKey scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
