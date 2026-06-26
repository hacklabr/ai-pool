package domain

import (
	"context"
	"time"
)

// =============================================================================
// Store Interfaces (Ports) — T2 da especificação.
//
// Estas interfaces abstraem os componentes de estado para permitir a troca de
// memoryStore (MVP, Opção A, 1 réplica) por RedisStore no futuro (Opção B,
// ≥2 réplicas) sem tocar no hot path do proxy.
//
// Consenso da fase de discussão (spec §3.1.3):
//   - MVP: apenas memoryStore implementada. NÃO importar go-redis.
//   - Gatilhos para migrar a Opção B: SLA duro, RPS>50/s, quotas distribuídas,
//     ou multi-tenancy real.
// =============================================================================

// KeyCandidate é uma chave elegível para atendimento de uma requisição.
// Retornado por LRUStore e AffinityStore.
type KeyCandidate struct {
	KeyID      ID
	ProviderID ID
	// DecryptedValue é o segredo desencriptado em memória APENAS durante a
	// requisição (deve ser zeroizado após uso). Providers da store nunca
	// armazenam este valor — quem o popula é o repositório de chaves.
	DecryptedValue []byte
	UserAgent      *string
}

// LRUStore mantém o ranking "least recently used" de chaves por provedor.
//
// Heurística pura (não persistida). Em 1 réplica o estado é consistente.
// Concorrência: a implementação deve ser thread-safe (mutex ou sync.Map).
//
// Spec §3.2.2: o last_used_at é atualizado NO MOMENTO DA SELEÇÃO (antes da
// chamada ao upstream) para que requisições simultâneas vejam a próxima chave
// menos recente. Races benignos (duas reqs pegando a mesma chave) são aceitos.
type LRUStore interface {
	// PickLeastUsed retorna a chave menos usada recentemente entre as chaves
	// elegíveis (active e não em cooldown) do provider, excluindo as em `exclude`.
	// Atualiza last_used_at=now para a chave escolhida. Retorna ErrNoKeysAvailable
	// se não há candidatas.
	PickLeastUsed(ctx context.Context, providerID ID, exclude []ID) (*KeyCandidate, error)

	// MarkUsed atualiza o last_used_at de uma chave (usado após sucesso ou em
	// seleções via afinidade). Idempotente.
	MarkUsed(ctx context.Context, keyID ID) error

	// OnKeyDisabled notifica o store que uma chave saiu do pool (auto-disable).
	// O store remove-a das candidatas LRU até ser reativada.
	OnKeyDisabled(ctx context.Context, keyID ID) error

	// OnKeyReenabled notifica que a chave voltou ao pool.
	OnKeyReenabled(ctx context.Context, keyID ID, providerID ID) error

	// EnsureCandidate garante que a chave está no pool de candidatas do LRU,
	// adicionando-a com last_used_at=zero (candidata a ser pickada primeiro) SOMENTE
	// se ainda não estiver presente. NÃO reseta o timestamp de chaves já conhecidas
	// (diferente de OnKeyReenabled). Usado para incluir chaves cadastradas após o
	// boot sem corromper o ranking LRU das existentes.
	EnsureCandidate(ctx context.Context, keyID ID, providerID ID) error
}

// AffinityStore mantém o mapeamento (usuário, conversa) → chave para aproveitar
// cache de contexto do LLM no provedor (diferencial do produto, spec §3.2.2).
//
// Mecanismo duplo:
//   - Primário: X-Conversation-ID (header opt-in do cliente).
//   - Fallback: hash SHA-256 (128 bits) do prefixo normalizado das mensagens.
//
// Continuidade = array anterior é PREFIXO ESTRITO do atual (n > k). Arrays
// idênticos (n == k) NÃO são continuidade (é repetição).
//
// Store POR USUÁRIO (sem colisão cross-user). TTL 30min. LRU eviction.
// Best-effort: se a chave alvo está em cooldown, cai para LRUStore.
// Segurança: o hash NUNCA aparece em logs/métricas; não persistir fingerprint.
type AffinityStore interface {
	// Lookup tenta resolver a chave de afinidade para (userID, conversationKey).
	// conversationKey é o ID explícito (X-Conversation-ID) OU o hash do prefixo.
	// Retorna ErrNoAffinity se não há entrada ou a chave alvo está indisponível.
	Lookup(ctx context.Context, userID ID, conversationKey string) (keyID ID, err error)

	// Remember registra a chave usada para (userID, conversationKey) após sucesso.
	// Renova o TTL da entrada. Atualiza o prefix_hash corrente para a próxima req.
	Remember(ctx context.Context, userID ID, conversationKey string, keyID ID, messageCount int) error

	// Invalidate remove a entrada de afinidade (ex.: chave foi desabilitada).
	Invalidate(ctx context.Context, userID ID, conversationKey string) error
}

// ConversationKey deriva o conversationKey apropriado para uma requisição.
// Esta função é utilitária (não interface) usada por quem orquestra afinidade.
// ConversationID explícito tem prioridade sobre hash de prefixo.
type ConversationResolver interface {
	// Resolve retorna o conversationKey (ou "" se não for possível estabelecer
	// continuidade, ex.: primeira mensagem sem ID).
	Resolve(conversationID string, normalizedPrefixHash string) string
}

// RateLimiter implementa token bucket por escopo (usuário ou token).
//
// MVP: in-memory (golang.org/x/time/rate). Spec §3.2.5.
// Permite burst controlado + média sustentada. Em caso de excesso, retorna
// ErrRateLimited com RetryAfter para o header HTTP 429.
type RateLimiter interface {
	// Allow checa e consome 1 token do bucket do escopo. Retorna allowed=false
	// e retryAfter > 0 se excedido. A implementação é responsável pela refill
	// preguiçosa.
	Allow(ctx context.Context, scope string) (allowed bool, retryAfter time.Duration, err error)

	// SetLimit configura a capacidade (burst) e taxa de reposição para um escopo.
	// Escopos sem configuração usam os defaults do sistema.
	SetLimit(ctx context.Context, scope string, burst int, ratePerMinute int) error
}

// TokenValidator valida tokens sk-aipp-... (DB hit por request, sem cache positivo).
//
// Consenso (spec §3.3.3, "D4" do Security Architect): MVP faz DB hit por request
// para garantir revogação instantânea e determinística ao desabilitar usuário.
// A interface abstrai para permitir RedisValidator futuro (se profiling justificar).
type TokenValidator interface {
	// Validate recebe o token completo sk-aipp-... e retorna o usuário dono
	// se válido, ou erro apropriado (ErrTokenInvalid/ErrTokenRevoked/ErrTokenExpired).
	// A implementação DB faz:
	//   1. Parse do formato sk-aipp-[key_id]-[secret].
	//   2. SELECT ... WHERE key_id = ? AND status='active'.
	//   3. hmac.Equal(token_hmac, hmacSHA256(secret, pepper)) — constant-time.
	//   4. Check user.Status == active (revogação imediata).
	Validate(ctx context.Context, rawToken string) (*User, *APIToken, error)
}

// =============================================================================
// Erros canônicos do domínio. São comparáveis com errors.Is.
// =============================================================================

// Sentinel errors. Definidos como valores (não tipos) para uso com errors.Is.
type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

// ErrNoKeysAvailable é retornado por LRUStore quando não há chaves elegíveis.
// O orquestrador de failover interpreta isto como "esgotou chaves do provider".
var ErrNoKeysAvailable = &sentinelError{"no keys available for provider"}

// ErrNoAffinity é retornado por AffinityStore quando não há entrada casando.
// Não é erro fatal — apenas sinaliza "use LRUStore".
var ErrNoAffinity = &sentinelError{"no affinity entry"}

// ErrRateLimited é retornado por RateLimiter quando o limite foi excedido.
// O caller deve incluir Retry-After na resposta 429.
var ErrRateLimited = &sentinelError{"rate limit exceeded"}

// ErrTokenInvalid formato inválido ou lookup vazio.
var ErrTokenInvalid = &sentinelError{"invalid api token"}

// ErrTokenRevoked token ativo no formato mas revogado/desabilitado.
var ErrTokenRevoked = &sentinelError{"token revoked or user disabled"}

// ErrTokenExpired token expirado.
var ErrTokenExpired = &sentinelError{"token expired"}
