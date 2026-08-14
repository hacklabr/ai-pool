package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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

// ApplyTestStatus aplica o resultado de um "teste manual" de chave (POST
// /api/web/admin/keys/{id}/test). Diferente de TransitionToAutoDisabled, NÃO
// filtra por status anterior — o teste é autoritativo: qualquer chave (mesmo
// manual_disabled) pode ser desativada por quota/auth ao ser testada.
//
// Quando ok=true: status=enabled, reseta error_count+consecutive_errors e limpa
// reason/disabled_at/disabled_until. Quando ok=false: status=auto_disabled e
// disabled_reason/disabled_until/last_error_hint são preenchidos.
//
// NÃO bloqueia chave manual_disabled em caso de sucesso (a semântica é "se a
// chave está funcionando, ative-a"). Caso o admin queira manter manual_disabled,
// não deve usar o botão Testar nesta chave.
//
// Idempotente: re-chamar com mesmo resultado não causa efeito colateral.
// Retorna ErrNotFound se a chave não existe na org.
func (r *KeyRepo) ApplyTestStatus(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	ok bool,
	reason domain.KeyDisabledReason,
	disabledUntil *time.Time,
	lastErrorHint *string,
) error {
	var q string
	var args []any
	if ok {
		q = `
			UPDATE provider_keys
			SET status = 'enabled',
			    disabled_reason = NULL,
			    disabled_at = NULL,
			    disabled_until = NULL,
			    last_error_at = NULL,
			    last_error_hint = NULL,
			    error_count = 0,
			    consecutive_errors = 0,
			    updated_at = now()
			WHERE organization_id = $1 AND id = $2`
		args = []any{int64(orgID), int64(keyID)}
	} else {
		q = `
			UPDATE provider_keys
			SET status = 'auto_disabled',
			    disabled_reason = $3,
			    disabled_at = now(),
			    disabled_until = $4,
			    last_error_at = now(),
			    last_error_hint = $5,
			    updated_at = now()
			WHERE organization_id = $1 AND id = $2`
		args = []any{
			int64(orgID), int64(keyID),
			string(reason),
			ptrToTimestamptz(disabledUntil),
			ptrToText(lastErrorHint),
		}
	}
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("KeyRepo.ApplyTestStatus: %w", err)
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

// ListAll retorna todas as chaves da organização (incluindo desabilitadas).
func (r *KeyRepo) ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.ProviderKey, error) {
	const q = `
		SELECT id, provider_id, organization_id, label, encrypted_value, encryption_key_id,
		       status, disabled_reason, disabled_at, disabled_until, error_count,
		       consecutive_errors, last_error_at, last_error_hint, last_used_at,
		       user_agent_override, created_at, updated_at
		FROM provider_keys
		WHERE organization_id = $1
		ORDER BY provider_id, label`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("KeyRepo.ListAll: %w", err)
	}
	defer rows.Close()

	var out []domain.ProviderKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("KeyRepo.ListAll scan: %w", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// UpdateMetadata altera apenas rótulo e user_agent_override de uma chave.
// O valor cifrado NÃO é alterado por este método.
func (r *KeyRepo) UpdateMetadata(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID, label string, uaOverride *string) error {
	const q = `
		UPDATE provider_keys
		SET label = $3, user_agent_override = $4, updated_at = now()
		WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(keyID), label, ptrToText(uaOverride))
	if err != nil {
		return fmt.Errorf("KeyRepo.UpdateMetadata: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete remove uma chave fisicamente.
func (r *KeyRepo) Delete(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID) error {
	const q = `DELETE FROM provider_keys WHERE organization_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(keyID))
	if err != nil {
		return fmt.Errorf("KeyRepo.Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
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

// TouchLastUsedBatch atualiza last_used_at para um conjunto de chaves em UMA
// única query (debounce 60s, spec §3.4.4).
//
// Em vez de atualizar last_used_at a cada request (write amplification), o
// flusher periódico acumula timestamps dirty do LRUStore e faz flush a cada
// 60s via esta função.
//
// timestamps: mapa keyID -> lastUsedAt. Apenas chaves presentes são atualizadas.
func (r *KeyRepo) TouchLastUsedBatch(ctx context.Context, timestamps map[domain.ID]time.Time) error {
	if len(timestamps) == 0 {
		return nil
	}
	const q = `
		UPDATE provider_keys AS k
		SET last_used_at = x.ts, updated_at = now()
		FROM (SELECT id, ts FROM unnest($1::bigint[], $2::timestamptz[])) AS x(id, ts)
		WHERE k.id = x.id`
	ids := make([]int64, 0, len(timestamps))
	tss := make([]time.Time, 0, len(timestamps))
	for id, ts := range timestamps {
		ids = append(ids, int64(id))
		tss = append(tss, ts)
	}
	_, err := r.pool.Exec(ctx, q, ids, tss)
	if err != nil {
		return fmt.Errorf("KeyRepo.TouchLastUsedBatch: %w", err)
	}
	return nil
}

// KeyTimestampEntry é o resultado de ListEnabledKeyTimestamps.
// Usado no boot para popular o LRUStore com os last_used_at reais do DB.
type KeyTimestampEntry struct {
	ProviderID domain.ID
	KeyID      domain.ID
	LastUsedAt time.Time // zero se NULL no DB (nunca usada)
}

// ListEnabledKeyTimestamps retorna (providerID, keyID, lastUsedAt) de todas as
// chaves 'enabled' da organização. Usado no boot para popular o LRUStore com
// os timestamps reais, permitindo reconstruir o ranking LRU após restart.
func (r *KeyRepo) ListEnabledKeyTimestamps(ctx context.Context, orgID domain.OrganizationID) ([]KeyTimestampEntry, error) {
	const q = `
		SELECT provider_id, id, COALESCE(last_used_at, '1970-01-01T00:00:01Z')
		FROM provider_keys
		WHERE organization_id = $1 AND status = 'enabled'`
	rows, err := r.pool.Query(ctx, q, int64(orgID))
	if err != nil {
		return nil, fmt.Errorf("KeyRepo.ListEnabledKeyTimestamps: %w", err)
	}
	defer rows.Close()

	var out []KeyTimestampEntry
	for rows.Next() {
		var (
			provID    int64
			keyID     int64
			lastUsedT pgtype.Timestamptz
		)
		if err := rows.Scan(&provID, &keyID, &lastUsedT); err != nil {
			return nil, fmt.Errorf("KeyRepo.ListEnabledKeyTimestamps scan: %w", err)
		}
		out = append(out, KeyTimestampEntry{
			ProviderID: domain.ID(provID),
			KeyID:      domain.ID(keyID),
			LastUsedAt: lastUsedT.Time,
		})
	}
	return out, rows.Err()
}
