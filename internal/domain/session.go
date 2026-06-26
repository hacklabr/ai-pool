package domain

import (
	"context"
	"errors"
	"time"
)

// =============================================================================
// Sessões web admin (cookie httpOnly) — segregadas do Bearer sk-aipp-... do proxy.
//
// Spec §3.3.5 / §3.6.5: login web usa SESSÃO server-side (não JWT stateless).
// Motivo: revogação imediata ao desabilitar usuário é trivial (deletar sessão
// do store). JWT exigiria denylist custosa.
//
// Implementação MVP: in-memory map protegido por RWMutex com TTL 12h e limpeza
// preguiçosa (lazy eviction na leitura). Perde-se em restart — aceitável para
// ~30 admins internos (eles simplesmente refazem login).
//
// ROADMAP: para multi-réplica (Opção B), trocar por RedisStore sem tocar no
// hot path do proxy (sessions só afetam admin/UI, não o /v1/*).
// =============================================================================

// Session representa uma sessão web admin autenticada.
type Session struct {
	ID        string // token opaco aleatório (base64url de 32 bytes)
	UserID    ID
	OrgID     OrganizationID
	Username  string
	Role      Role
	CreatedAt time.Time
	ExpiresAt time.Time // TTL absoluto
	// LastSeenAt para sliding expiration (renovada a cada req). MVP: usa TTL
	// absoluto fixo (12h) — mais simples, previne sessões imortais.
	LastSeenAt time.Time
	CSRFToken  string // token CSRF double-submit associado a esta sessão
}

// SessionStore é a PORTA do armazenamento de sessões.
//
// Abstração permite troca in-memory → Redis sem tocar código de negócio.
type SessionStore interface {
	// Create insere uma nova sessão. Retorna erro se o ID já existe (improvável
	// com 32 bytes de entropia — caller pode retentar).
	Create(ctx context.Context, s *Session) error

	// Get retorna a sessão pelo ID. Retorna ErrSessionNotFound se inexistente
	// ou expirada (limpeza lazy: a entrada é removida se expirou).
	Get(ctx context.Context, id string) (*Session, error)

	// Delete remove a sessão (logout). Idempotente.
	Delete(ctx context.Context, id string) error

	// DeleteAllForUser remove TODAS as sessões de um usuário. Usado em:
	//   - Desabilitação de usuário (revogação imediata, spec §3.3.3).
	//   - Reset de senha (invalida sessões antigas).
	DeleteAllForUser(ctx context.Context, userID ID) error
}

// Erros de sessão.
var (
	ErrSessionNotFound = errors.New("session not found or expired")
)

// IsExpired retorna true se a sessão passou do TTL.
func (s *Session) IsExpired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// SessionTTLDefault é o TTL absoluto padrão (12h).
// Sliding expiration seria mais conveniente mas aceitamos re-login diário
// para admins internos (previne sessões imortais em máquinas compartilhadas).
const SessionTTLDefault = 12 * time.Hour
