package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenRepo persiste e consulta tokens sk-aipp-... (spec §3.4.1 item 3).
//
// O hot path de autenticação chama GetActiveByKeyID a cada request (DB hit por
// request, sem cache positivo — spec §3.3.3 D4). O índice partial UNIQUE em
// key_id (WHERE revoked_at IS NULL AND status='active') torna este lookup O(1).
type TokenRepo struct {
	pool *pgxpool.Pool
}

// NewTokenRepo cria o repositório.
func NewTokenRepo(pool *pgxpool.Pool) *TokenRepo { return &TokenRepo{pool: pool} }

// Create insere um novo token. tokenHmac deve ser o HMAC-SHA256(secret, pepper)
// pré-calculado pela camada de criptografia (F7). keyID é a parte pública
// (8 chars, gerada com crypto/rand).
// Retorna o ID gerado.
func (r *TokenRepo) Create(ctx context.Context, t *domain.APIToken) (domain.ID, error) {
	if t == nil {
		return 0, errors.New("TokenRepo.Create: nil token")
	}
	const q = `
		INSERT INTO api_tokens (user_id, organization_id, key_id, token_hmac, name, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, 'active'), $7)
		RETURNING id`
	var id int64
	err := r.pool.QueryRow(ctx, q,
		int64(t.UserID),
		int64(t.OrgID),
		t.KeyID,
		[]byte(t.TokenHMAC),
		t.Name,
		string(t.Status),
		ptrToTimestamptz(t.ExpiresAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("TokenRepo.Create: %w", err)
	}
	return domain.ID(id), nil
}

// GetActiveByKeyID é o lookup O(1) do hot path de auth.
//
// Retorna apenas tokens ativos e não-revogados (a partial unique index já
// filtra). O caller (TokenValidator em F4) ainda deve:
//  1. Comparar tokenHMAC via hmac.Equal (constant-time) com o HMAC do secret
//     enviado pelo cliente. NUNCA usar == (timing attack).
//  2. Verificar ExpiresAt.
//  3. Verificar user.Status == active (revogação imediata).
//
// Retorna (nil, ErrNotFound) se o keyID não casa nenhum token ativo. O caller
// converte isto em domain.ErrTokenInvalid.
func (r *TokenRepo) GetActiveByKeyID(ctx context.Context, keyID string) (*domain.APIToken, error) {
	const q = `
		SELECT id, user_id, organization_id, key_id, token_hmac, name, status,
		       created_at, expires_at, last_used_at, revoked_at
		FROM api_tokens
		WHERE key_id = $1 AND status = 'active' AND revoked_at IS NULL`
	row := r.pool.QueryRow(ctx, q, keyID)
	t, err := scanToken(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("TokenRepo.GetActiveByKeyID: %w", err)
	}
	return t, nil
}

// GetByID carrega um token pela PK (uso em admin UI: listar tokens do usuário).
func (r *TokenRepo) GetByID(ctx context.Context, orgID domain.OrganizationID, id domain.ID) (*domain.APIToken, error) {
	const q = `
		SELECT id, user_id, organization_id, key_id, token_hmac, name, status,
		       created_at, expires_at, last_used_at, revoked_at
		FROM api_tokens
		WHERE organization_id = $1 AND id = $2`
	row := r.pool.QueryRow(ctx, q, int64(orgID), int64(id))
	t, err := scanToken(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("TokenRepo.GetByID: %w", err)
	}
	return t, nil
}

// Revoke marca o token como revogado (soft-delete). Irreversível.
// A partial unique index libera o key_id para reuso (se desejado).
func (r *TokenRepo) Revoke(ctx context.Context, orgID domain.OrganizationID, id domain.ID) error {
	const q = `
		UPDATE api_tokens
		SET status = 'revoked', revoked_at = now(), updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND revoked_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, int64(orgID), int64(id))
	if err != nil {
		return fmt.Errorf("TokenRepo.Revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByUser retorna os tokens de um usuário (admin UI / "meus tokens").
// Não retorna o token_hmac (secredo) — apenas metadados.
type TokenListItem struct {
	ID         domain.ID
	KeyID      string
	Name       string
	Status     domain.APITokenStatus
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (r *TokenRepo) ListByUser(ctx context.Context, orgID domain.OrganizationID, userID domain.ID) ([]TokenListItem, error) {
	const q = `
		SELECT id, key_id, name, status, created_at, expires_at, last_used_at, revoked_at
		FROM api_tokens
		WHERE organization_id = $1 AND user_id = $2
		ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, int64(orgID), int64(userID))
	if err != nil {
		return nil, fmt.Errorf("TokenRepo.ListByUser: %w", err)
	}
	defer rows.Close()

	var items []TokenListItem
	for rows.Next() {
		var (
			it        TokenListItem
			status    string
			expiresT  pgtype.Timestamptz
			lastUsedT pgtype.Timestamptz
			revokedT  pgtype.Timestamptz
		)
		if err := rows.Scan(&it.ID, &it.KeyID, &it.Name, &status, &it.CreatedAt,
			&expiresT, &lastUsedT, &revokedT); err != nil {
			return nil, fmt.Errorf("TokenRepo.ListByUser scan: %w", err)
		}
		it.Status = domain.APITokenStatus(status)
		it.ExpiresAt = timestamptzToPtr(expiresT)
		it.LastUsedAt = timestamptzToPtr(lastUsedT)
		it.RevokedAt = timestamptzToPtr(revokedT)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("TokenRepo.ListByUser rows: %w", err)
	}
	return items, nil
}

// TouchLastUsedBatch atualiza last_used_at (e opcionalmente last_used_ip) para
// um conjunto de tokens em UMA única query.
//
// Esta é a implementação do DEBOUNCE de last_used_at (spec §3.4.4): em vez de
// atualizar last_used_at a cada request (write amplification), a aplicação
// acumula IDs atualizados num "dirty set" em memória e faz flush a cada 60s
// via esta função. Aceita-se imprecisão ≤60s em troca de MUITO menos writes.
//
// tokenIDs: mapa tokenID -> (IP opcional). now: timestamp a gravar.
func (r *TokenRepo) TouchLastUsedBatch(ctx context.Context, tokenIDs map[domain.ID]net.IP, now time.Time) error {
	if len(tokenIDs) == 0 {
		return nil
	}
	// UNNEST permite atualizar N linhas em uma única round-trip.
	const q = `
		UPDATE api_tokens AS t
		SET last_used_at = $2,
		    last_used_ip = x.ip,
		    updated_at = now()
		FROM (SELECT id, ip FROM unnest($1::bigint[], $3::inet[])) AS x(id, ip)
		WHERE t.id = x.id`
	ids := make([]int64, 0, len(tokenIDs))
	ips := make([]net.IP, 0, len(tokenIDs))
	for id, ip := range tokenIDs {
		ids = append(ids, int64(id))
		ips = append(ips, ip)
	}
	_, err := r.pool.Exec(ctx, q, ids, now, ips)
	if err != nil {
		return fmt.Errorf("TokenRepo.TouchLastUsedBatch: %w", err)
	}
	return nil
}

// scanToken mapeia coluna→domain.APIToken (DRY entre Get*).
func scanToken(row pgx.Row) (*domain.APIToken, error) {
	var (
		t         domain.APIToken
		userID    int64
		orgID     int64
		hmac      []byte
		status    string
		expiresT  pgtype.Timestamptz
		lastUsedT pgtype.Timestamptz
		revokedT  pgtype.Timestamptz
	)
	err := row.Scan(
		&t.ID, &userID, &orgID, &t.KeyID, &hmac, &t.Name, &status,
		&t.CreatedAt, &expiresT, &lastUsedT, &revokedT,
	)
	if err != nil {
		return nil, err
	}
	t.UserID = domain.ID(userID)
	t.OrgID = domain.OrganizationID(orgID)
	t.TokenHMAC = hmac
	t.Status = domain.APITokenStatus(status)
	t.ExpiresAt = timestamptzToPtr(expiresT)
	t.LastUsedAt = timestamptzToPtr(lastUsedT)
	t.RevokedAt = timestamptzToPtr(revokedT)
	return &t, nil
}

// =============================================================================
// Stubs para fases futuras:
//
//   - TouchLastUsedBatchIPs(ids, ipMap, now)   -> já implementado acima.
//   - PurgeExpired(before)                    -> job diário (F6).
//   - CountActiveByUser(userID)               -> admin UI (F8).
// =============================================================================
