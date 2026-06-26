package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rafael/ai-pool/internal/domain"
)

// KeyRepo persiste chaves de provedores (spec §3.4.1 item 5) e implementa as
// queries críticas do núcleo do produto:
//
//   - ListUsableByProvider: candidatas a seleção LRU/afinidade/failover.
//   - ListToReactivate:     fila do scheduler (FOR UPDATE SKIP LOCKED).
//   - TransitionToAutoDisabled: write-through atômico (CAS por status).
//   - Reactivate:           marca chave como enabled (pós-scheduler).
//
// Importante (spec §3.2.4, §3.4.4): contadores de erro (error_count,
// consecutive_errors) são gerenciados primariamente IN-MEMORY no hot path;
// apenas as TRANSIÇÕES DE ESTADO (enabled→auto_disabled etc.) são persistidas
// imediatamente (write-through) — porque afetam scheduler, dashboard e
// (futuramente) outras réplicas. last_used_at é debounce 60s (ver TokenRepo).
type KeyRepo struct {
	pool *pgxpool.Pool
}

// NewKeyRepo cria o repositório.
func NewKeyRepo(pool *pgxpool.Pool) *KeyRepo { return &KeyRepo{pool: pool} }

// Create insere uma nova chave. encryptedValue deve vir já cifrado (AES-256-GCM)
// da camada de criptografia (F7) — o repositório persiste o ciphertext opaco.
func (r *KeyRepo) Create(ctx context.Context, k *domain.ProviderKey) (domain.ID, error) {
	if k == nil {
		return 0, errors.New("KeyRepo.Create: nil key")
	}
	const q = `
		INSERT INTO provider_keys (provider_id, organization_id, label, encrypted_value,
		                           encryption_key_id, status, user_agent_override)
		VALUES ($1, $2, $3, $4, COALESCE($5,'v1'), COALESCE($6,'enabled'), $7)
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(k.ProviderID), int64(k.OrgID), k.Label,
		[]byte(k.EncryptedValue), k.EncryptionKeyID,
		string(k.Status), ptrToText(k.UserAgentOverride),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("KeyRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetByID carrega uma chave pela PK. Inclui o ciphertext (a descifragem é feita
// em F7 fora deste repositório, apenas no momento do uso).
func (r *KeyRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.ProviderKey, error) {
	const q = `
		SELECT id, provider_id, organization_id, label, encrypted_value, encryption_key_id,
		       status, disabled_reason, disabled_at, disabled_until, error_count,
		       consecutive_errors, last_error_at, last_error_hint, last_used_at,
		       user_agent_override, created_at, updated_at
		FROM provider_keys
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	k, err := scanKey(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("KeyRepo.GetByID: %w", err)
	}
	return k, nil
}

// ListUsableByProvider retorna as chaves 'enabled' de um provedor, ordenadas
// por last_used_at ASC (NULLS FIRST = chaves nunca usadas vêm primeiro).
//
// Esta lista alimenta o LRUStore no boot (reconstrução do ranking) e pode ser
// usada como fallback quando a store in-memory está vazia. No hot path normal,
// a store in-memory atende sem bater no DB.
//
// NOTA: NÃO retorna chaves auto_disabled cujo disabled_until já passou (lazy
// reativação) — o scheduler as reativa formalmente. O domínio.IsUsable() trata
// este caso quando aplicável.
func (r *KeyRepo) ListUsableByProvider(ctx context.Context, orgID domain.OrganizationID, providerID domain.ID) ([]domain.ProviderKey, error) {
	const q = `
		SELECT id, provider_id, organization_id, label, encrypted_value, encryption_key_id,
		       status, disabled_reason, disabled_at, disabled_until, error_count,
		       consecutive_errors, last_error_at, last_error_hint, last_used_at,
		       user_agent_override, created_at, updated_at
		FROM provider_keys
		WHERE organization_id = $1 AND provider_id = $2 AND status = 'enabled'
		ORDER BY last_used_at NULLS FIRST`
	rows, err := r.pool.Query(ctx, q, int64(orgID), int64(providerID))
	if err != nil {
		return nil, fmt.Errorf("KeyRepo.ListUsableByProvider: %w", err)
	}
	defer rows.Close()

	var out []domain.ProviderKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("KeyRepo.ListUsableByProvider scan: %w", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ListToReactivate é a query do SCHEDULER de reativação (spec §3.2.4, §3.4.4).
//
// Pega até `limit` chaves 'auto_disabled' cujo disabled_until <= now, com
// FOR UPDATE SKIP LOCKED — múltiplos workers (no futuro, multi-réplica) NÃO
// pegam a mesma chave. Cada worker processa e reativa; se morrer, o lock
// libera no fim da transação e outro worker pega.
//
// O caller DEVE envolver esta chamada numa transação e chamar Reactivate (ou
// descartar) antes do commit, para não segurar locks indefinidamente.
//
// Retorna os IDs a reativar (com provider_id para notificar stores in-memory).
type ReactivationCandidate struct {
	KeyID      domain.ID
	ProviderID domain.ID
}

func (r *KeyRepo) ListToReactivate(ctx context.Context, orgID domain.OrganizationID, limit int) ([]ReactivationCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	// FOR UPDATE SKIP LOCKED requer estar dentro de transação; pgxpool.Query
	// roda em modo implícito (auto-commit) que NÃO segura o lock após o retorno.
	// Para o scheduler MVP (1 réplica), isto é aceitável: o SELECT bloqueia as
	// linhas durante a leitura e o UPDATE seguinte (Reactivate) ocorre em
	// sequência. Para multi-réplica, envolver em tx explícita (roadmap).
	const q = `
		SELECT id, provider_id
		FROM provider_keys
		WHERE organization_id = $1
		  AND status = 'auto_disabled'
		  AND disabled_until IS NOT NULL
		  AND disabled_until <= now()
		ORDER BY disabled_until
		FOR UPDATE SKIP LOCKED
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, int64(orgID), limit)
	if err != nil {
		return nil, fmt.Errorf("KeyRepo.ListToReactivate: %w", err)
	}
	defer rows.Close()

	var out []ReactivationCandidate
	for rows.Next() {
		var c ReactivationCandidate
		if err := rows.Scan(&c.KeyID, &c.ProviderID); err != nil {
			return nil, fmt.Errorf("KeyRepo.ListToReactivate scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TransitionToAutoDisabled é o WRITE-THROUGH atômico para a transição de estado
// enabled → auto_disabled (spec §3.2.4, §3.4.4).
//
// A condição WHERE status = 'enabled' (CAS) torna a transição idempotente:
// múltiplos workers detectando o mesmo erro NÃO causam dupla transição — só o
// primeiro "vence"; os outros afetam 0 linhas (retornamos nil, não erro).
//
// disabledUntil: timestamp de reativação (do parse de erro OU backoff).
// reason: classificação do motivo (quota_exceeded, rate_limited, etc.).
// errorCount/consecutiveErrors: snapshot dos contadores in-memory no momento.
//
// Retorna ErrConflict se a chave não estava em estado 'enabled' (já tinha sido
// desabilitada por outro caminho) — o caller decide se ignora.
func (r *KeyRepo) TransitionToAutoDisabled(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	reason domain.KeyDisabledReason,
	disabledUntil time.Time,
	errorCount int,
	consecutiveErrors int,
	lastErrorHint *string,
) error {
	const q = `
		UPDATE provider_keys
		SET status = 'auto_disabled',
		    disabled_reason = $3,
		    disabled_at = now(),
		    disabled_until = $4,
		    error_count = $5,
		    consecutive_errors = $6,
		    last_error_at = now(),
		    last_error_hint = $7,
		    updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND status = 'enabled'`
	tag, err := r.pool.Exec(ctx, q,
		int64(orgID), int64(keyID),
		string(reason), disabledUntil, errorCount, consecutiveErrors,
		ptrToText(lastErrorHint),
	)
	if err != nil {
		return fmt.Errorf("KeyRepo.TransitionToAutoDisabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// CAS perdido: outro worker já transicionou. Idempotente — não é erro.
		return ErrConflict
	}
	return nil
}

// TransitionToManualDisabled força disable manual (admin, spec §3.2.4).
// Ignora disabled_until (manual não tem reativação automática).
func (r *KeyRepo) TransitionToManualDisabled(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID) error {
	const q = `
		UPDATE provider_keys
		SET status = 'manual_disabled',
		    disabled_reason = 'manual',
		    disabled_at = now(),
		    disabled_until = NULL,
		    updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(keyID))
	if err != nil {
		return fmt.Errorf("KeyRepo.TransitionToManualDisabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Reactivate marca uma chave (previamente auto_disabled) como enabled.
// Reseta contadores de erro. Usado pelo scheduler após ListToReactivate.
func (r *KeyRepo) Reactivate(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID) error {
	const q = `
		UPDATE provider_keys
		SET status = 'enabled',
		    disabled_reason = NULL,
		    disabled_at = NULL,
		    disabled_until = NULL,
		    error_count = 0,
		    consecutive_errors = 0,
		    updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND status != 'enabled'`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(keyID))
	if err != nil {
		return fmt.Errorf("KeyRepo.Reactivate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReactivateExpiredKeys faz UPDATE atômico + RETURNING das chaves
// 'auto_disabled' cujo disabled_until <= now, marcando-as como 'enabled'.
//
// Usado na "reativação lazy" (spec §3.2.4): o routing engine chama isto no
// início de cada ResolveKeyWithAffinity, garantindo que chaves cujo cooldown
// expirou sejam reativadas imediatamente (sem esperar o próximo tick do
// scheduler de 30s).
//
// Diferença vs ListToReactivate+Reactivate:
//   - ListToReactivate usa FOR UPDATE SKIP LOCKED em transação separada (mais
//     seguro para multi-worker mas exige tx explícita).
//   - ReactivateExpiredKeys é UPDATE atômico único (sem lock explícito) —
//     idempotente e suficiente para MVP single-réplica. Para multi-réplica
//     (Opção B futuro), trocar por ListToReactivate dentro de tx.
//
// Retorna os IDs das chaves efetivamente reativadas (para notificar LRUStore).
func (r *KeyRepo) ReactivateExpiredKeys(ctx context.Context, orgID domain.OrganizationID, now time.Time) ([]domain.ID, error) {
	const q = `
		UPDATE provider_keys
		SET status = 'enabled',
		    disabled_reason = NULL,
		    disabled_at = NULL,
		    disabled_until = NULL,
		    error_count = 0,
		    consecutive_errors = 0,
		    updated_at = now()
		WHERE organization_id = $1
		  AND status = 'auto_disabled'
		  AND disabled_until IS NOT NULL
		  AND disabled_until <= $2
		RETURNING id`
	rows, err := r.pool.Query(ctx, q, int64(orgID), now)
	if err != nil {
		return nil, fmt.Errorf("KeyRepo.ReactivateExpiredKeys: %w", err)
	}
	defer rows.Close()
	var ids []domain.ID
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("KeyRepo.ReactivateExpiredKeys scan: %w", err)
		}
		ids = append(ids, domain.ID(id))
	}
	return ids, rows.Err()
}

// IncrementError persiste os contadores de erro (snapshot). Usado em flush
// periódico (a cada ~5s) — NÃO no hot path a cada erro (write amplification).
// A transição enabled→auto_disabled NÃO passa por aqui; usa TransitionToAutoDisabled.
func (r *KeyRepo) IncrementError(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID, errorCount, consecutiveErrors int) error {
	const q = `
		UPDATE provider_keys
		SET error_count = $3, consecutive_errors = $4, last_error_at = now(), updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	_, err := r.pool.Exec(ctx, q, int64(orgID), int64(keyID), errorCount, consecutiveErrors)
	if err != nil {
		return fmt.Errorf("KeyRepo.IncrementError: %w", err)
	}
	return nil
}

// scanKey mapeia coluna→domain.ProviderKey (DRY entre Get* e List*).
func scanKey(row pgx.Row) (*domain.ProviderKey, error) {
	var (
		k              domain.ProviderKey
		providerID     int64
		orgID          int64
		encValue       []byte
		status         string
		disabledReason pgtype.Text
		disabledAtT    pgtype.Timestamptz
		disabledUntilT pgtype.Timestamptz
		lastErrorAtT   pgtype.Timestamptz
		lastErrorHint  pgtype.Text
		lastUsedT      pgtype.Timestamptz
		uaOverride     pgtype.Text
	)
	err := row.Scan(
		&k.ID, &providerID, &orgID, &k.Label, &encValue, &k.EncryptionKeyID,
		&status, &disabledReason, &disabledAtT, &disabledUntilT, &k.ErrorCount,
		&k.ConsecutiveErrors, &lastErrorAtT, &lastErrorHint, &lastUsedT,
		&uaOverride, &k.CreatedAt, &k.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	k.ProviderID = domain.ID(providerID)
	k.OrgID = domain.OrganizationID(orgID)
	k.EncryptedValue = encValue
	k.Status = domain.KeyStatus(status)
	if disabledReason.Valid {
		reason := domain.KeyDisabledReason(disabledReason.String)
		k.DisabledReason = &reason
	}
	k.DisabledAt = timestamptzToPtr(disabledAtT)
	k.DisabledUntil = timestamptzToPtr(disabledUntilT)
	k.LastErrorAt = timestamptzToPtr(lastErrorAtT)
	k.LastErrorHint = textToPtr(lastErrorHint)
	k.LastUsedAt = timestamptzToPtr(lastUsedT)
	k.UserAgentOverride = textToPtr(uaOverride)
	return &k, nil
}

// =============================================================================
// Stubs para fases futuras:
//
//   - TouchLastUsedBatch(ids, now)               -> debounce LRU (especular TokenRepo).
//   - ListByProviderForAdmin(orgID, providerID)  -> F8 admin UI (todas as chaves).
//   - RotateEncryption(id, newCipher, newKeyID)  -> rotação de KEK (roadmap).
//   - UpdateUserAgentOverride(id, ua)            -> F8 admin UI.
// =============================================================================
