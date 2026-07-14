package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// RoutingEngine — implementa domain.RoutingEngine.
//
// Integra LRUStore + AffinityStore + KeyRepo (via portas) para resolver a chave
// ideal para uma requisição, considerando:
//
//	1. Afinidade de cache de contexto (X-Conversation-ID preferido, hash fallback).
//	2. LRU (least-recently-used) entre as chaves habilitadas do provider.
//	3. Lazy reativação de chaves auto_disabled cujo disabled_until passou.
//	4. Exclusão de chaves já tentadas (failover chain).
//
// Decisões:
//   - DecryptedValue é populado pelo decryptor injetado (Identity dev, AES-GCM prod).
//   - Afinidade é best-effort: se a chave alvo está em cooldown/desabilitada,
//     cai para LRU.
//   - LRU é heurística pura (não persistida); o store in-memory cuida do ranking.
// =============================================================================

// EngineDeps Porta mínima dos repositórios necessários ao engine.
// Permite mock em testes.
type EngineDeps interface {
	// GetActiveProviderForModel retorna o provider ativo que oferece o model.
	GetActiveProviderForModel(ctx context.Context, orgID domain.OrganizationID, model string) (*domain.Provider, error)
	// ListUsableByProvider retorna chaves habilitadas do provider.
	ListUsableByProvider(ctx context.Context, orgID domain.OrganizationID, providerID domain.ID) ([]domain.ProviderKey, error)
	// ReactivateExpiredKeys faz UPDATE+RETURNING das chaves auto_disabled cujo
	// disabled_until <= now. Retorna os IDs reativados (para notificar LRUStore).
	ReactivateExpiredKeys(ctx context.Context, orgID domain.OrganizationID, now time.Time) ([]domain.ID, error)
}

// RoutingEngine implementa domain.RoutingEngine.
type RoutingEngine struct {
	deps        EngineDeps
	decryptor   domain.KeyDecryptor
	lru         domain.LRUStore
	affinity    domain.AffinityStore
	autoDisable *AutoDisableManager
	logger      *slog.Logger
	now         func() time.Time
}

// EngineOption configura o RoutingEngine no construtor.
type EngineOption func(*RoutingEngine)

// WithEngineLogger sobrescreve o logger default.
func WithEngineLogger(l *slog.Logger) EngineOption {
	return func(e *RoutingEngine) { e.logger = l }
}

// WithEngineClock injeta um clock (testes determinísticos).
func WithEngineClock(now func() time.Time) EngineOption {
	return func(e *RoutingEngine) { e.now = now }
}

// NewRoutingEngine cria o engine com todos os adapters.
//
// autoDisable pode ser nil (caso não se queira tracking de erros — útil em
// testes isolados de LRU/afinidade).
func NewRoutingEngine(
	deps EngineDeps,
	decryptor domain.KeyDecryptor,
	lru domain.LRUStore,
	affinity domain.AffinityStore,
	autoDisable *AutoDisableManager,
	logger *slog.Logger,
	opts ...EngineOption,
) *RoutingEngine {
	e := &RoutingEngine{
		deps:        deps,
		decryptor:   decryptor,
		lru:         lru,
		affinity:    affinity,
		autoDisable: autoDisable,
		logger:      logger,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ResolveKey implementa domain.KeyResolver (forma simples, sem afinidade).
// Delega para ResolveKeyWithAffinity com conversationKey vazio e sem exclude.
// Útil para callers que não têm contexto de conversa.
func (e *RoutingEngine) ResolveKey(ctx context.Context, orgID domain.OrganizationID, model string) (*domain.KeyResolution, error) {
	return e.ResolveKeyWithAffinity(ctx, orgID, 0, model, "", nil)
}

// ResolveKeyWithAffinity implementa domain.RoutingEngine.
//
// Fluxo:
//  1. Lazy reativação: chama ReactivateExpiredKeys (UPDATE atômico das chaves
//     auto_disabled cujo disabled_until passou). Notifica LRUStore.
//  2. Resolve provider pelo model.
//  3. Se há conversationKey, tenta afinidade:
//     a. AffinityStore.Lookup(userID, conversationKey) → keyID alvo.
//     b. Se keyID alvo está na lista de chaves usable E não está em exclude → usa.
//     c. Senão (indisponível/excluída) → cai para LRU.
//  4. LRU: ListUsableByProvider + PickLeastUsed(exclude).
//  5. Decrypt valor.
func (e *RoutingEngine) ResolveKeyWithAffinity(
	ctx context.Context,
	orgID domain.OrganizationID,
	userID domain.ID,
	model string,
	conversationKey string,
	exclude []domain.ID,
) (*domain.KeyResolution, error) {
	if model == "" {
		return nil, fmt.Errorf("routing engine: empty model")
	}

	// 1. Lazy reativação — best-effort (erros são logados mas não fatais).
	if e.deps != nil {
		now := e.now()
		if reactivated, err := e.deps.ReactivateExpiredKeys(ctx, orgID, now); err == nil {
			for _, kid := range reactivated {
				// Precisamos do providerID para OnKeyReenabled. Como não temos
				// ele aqui, passamos 0 e o LRUStore trata por lookup interno.
				// Na implementação MemoryLRUStore, OnKeyReenabled recebe
				// providerID para inserir no map correto — vamos buscar via
				// ListUsableByProvider logo abaixo (a chave já estará 'enabled').
				// Para notificar corretamente o LRUStore, fazemos o Reenable
				// APÓS carregar a lista de chaves do provider.
				_ = kid
			}
		} else {
			e.logger.Debug("lazy reactivation skipped", "err", err)
		}
	}

	// 2. Provider que oferece este model.
	provider, err := e.deps.GetActiveProviderForModel(ctx, orgID, model)
	if err != nil {
		if errors.Is(err, domain.ErrModelNotFound) {
			return nil, domain.ErrModelNotFound
		}
		return nil, fmt.Errorf("routing engine: provider lookup: %w", err)
	}

	// 3. Lista de chaves usable.
	keys, err := e.deps.ListUsableByProvider(ctx, orgID, provider.ID)
	if err != nil {
		return nil, fmt.Errorf("routing engine: keys lookup: %w", err)
	}
	if len(keys) == 0 {
		return nil, domain.ErrNoKeysAvailable
	}

	// Constrói mapa keyID → ProviderKey + conjunto usable para lookup rápido.
	keyByID := make(map[domain.ID]*domain.ProviderKey, len(keys))
	usableIDs := make([]domain.ID, 0, len(keys))
	for i := range keys {
		k := &keys[i]
		keyByID[k.ID] = k
		usableIDs = append(usableIDs, k.ID)
		// Garante que a chave está no pool LRU (adiciona só se ausente, sem
		// resetar timestamp das já conhecidas — bug anterior: OnKeyReenabled
		// resetava TODAS as chaves a cada request, corrompendo o ranking).
		if e.lru != nil {
			_ = e.lru.EnsureCandidate(ctx, k.ID, provider.ID)
		}
	}

	// excludedSet para filtra rápido.
	excluded := make(map[domain.ID]struct{}, len(exclude))
	for _, id := range exclude {
		excluded[id] = struct{}{}
	}

	// 4. Tenta afinidade PRIMEIRO (se conversationKey presente).
	if conversationKey != "" && e.affinity != nil && userID != 0 {
		affKeyID, err := e.affinity.Lookup(ctx, userID, conversationKey)
		if err == nil && affKeyID != 0 {
			// Verifica se a chave alvo está usable e não foi excluída.
			if k, ok := keyByID[affKeyID]; ok {
				if _, isExcluded := excluded[affKeyID]; !isExcluded {
					// HIT de afinidade — usa esta chave.
					decrypted, derr := e.decryptor.Decrypt(ctx, *k)
					if derr != nil {
						return nil, fmt.Errorf("routing engine: affinity decrypt: %w", derr)
					}
					// Marca como usada no LRU (atualiza last_used_at).
					if e.lru != nil {
						_ = e.lru.MarkUsed(ctx, affKeyID)
					}
					e.logger.Info("CONTEXT CACHE HIT",
						"key_id", affKeyID,
						"source", conversationSource(conversationKey),
					)
					return &domain.KeyResolution{
						Provider:       *provider,
						Key:            *k,
						DecryptedValue: decrypted,
						ViaAffinity:    true,
					}, nil
				}
			}
			// A chave alvo foi desabilitada ou excluída — invalida afinidade e cai p/ LRU.
			_ = e.affinity.Invalidate(ctx, userID, conversationKey)
			e.logger.Debug("affinity stale, falling back to LRU", "key_id", affKeyID)
		}
	}

	// 5. LRU: PickLeastUsed com exclusões.
	if e.lru != nil {
		cand, err := e.lru.PickLeastUsed(ctx, provider.ID, exclude)
		if err == nil && cand != nil {
			// Verifica se ainda está usable (LRU pode ter entry stale).
			if k, ok := keyByID[cand.KeyID]; ok {
				if _, isExcluded := excluded[cand.KeyID]; !isExcluded {
					decrypted, derr := e.decryptor.Decrypt(ctx, *k)
					if derr != nil {
						return nil, fmt.Errorf("routing engine: LRU decrypt: %w", derr)
					}
					e.logger.Info("CONTEXT CACHE MISS",
						"key_id", cand.KeyID,
						"reason", "lru",
						"affinity_skipped", conversationKey == "",
					)
					return &domain.KeyResolution{
						Provider:       *provider,
						Key:            *k,
						DecryptedValue: decrypted,
					}, nil
				}
			}
			// LRU retornou chave que não está mais usable — notifica store.
			_ = e.lru.OnKeyDisabled(ctx, cand.KeyID)
		}
	}

	// 6. Fallback: primeira chave usable não excluída (sem LRU ou LRU vazio).
	for _, id := range usableIDs {
		if _, isExcluded := excluded[id]; isExcluded {
			continue
		}
		k := keyByID[id]
		decrypted, derr := e.decryptor.Decrypt(ctx, *k)
		if derr != nil {
			return nil, fmt.Errorf("routing engine: fallback decrypt: %w", derr)
		}
		if e.lru != nil {
			_ = e.lru.MarkUsed(ctx, id)
		}
		return &domain.KeyResolution{
			Provider:       *provider,
			Key:            *k,
			DecryptedValue: decrypted,
		}, nil
	}

	// Todas foram excluídas → esgotou.
	return nil, domain.ErrNoKeysAvailable
}

// OnKeySucceeded implementa domain.RoutingEngine.
// Atualiza afinidade (Remember) + LRU (MarkUsed) + reseta contador auto-disable.
func (e *RoutingEngine) OnKeySucceeded(
	ctx context.Context,
	orgID domain.OrganizationID,
	userID domain.ID,
	conversationKey string,
	keyID domain.ID,
	messageCount int,
) error {
	if e.lru != nil {
		_ = e.lru.MarkUsed(ctx, keyID)
	}
	// Só registra afinidade se há conversationKey (continuidade estabelecida).
	if conversationKey != "" && e.affinity != nil && userID != 0 {
		_ = e.affinity.Remember(ctx, userID, conversationKey, keyID, messageCount)
	}
	if e.autoDisable != nil {
		e.autoDisable.ResetCounter(orgID, keyID)
	}
	return nil
}

// OnKeyFailed implementa domain.RoutingEngine.
// Registra erro no AutoDisableManager (que pode disparar transição para
// auto_disabled se threshold atingido).
func (e *RoutingEngine) OnKeyFailed(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	providerID domain.ID,
	providerSlug string,
	statusCode int,
	errBody []byte,
) error {
	if e.autoDisable == nil {
		return nil
	}
	return e.autoDisable.RecordError(ctx, orgID, keyID, providerID, providerSlug, statusCode, errBody)
}

// conversationSource extrai a origem do conversationKey ("header" vs "hash")
// para logging. NUNCA loga o conteúdo do hash. Prefixos: "id:" (header
// X-Session-Id/X-Conversation-ID) ou "h:" (hash do prefixo das mensagens).
func conversationSource(conversationKey string) string {
	if len(conversationKey) >= 3 && conversationKey[0] == 'i' && conversationKey[1] == 'd' && conversationKey[2] == ':' {
		return "header"
	}
	if len(conversationKey) >= 2 && conversationKey[0] == 'h' && conversationKey[1] == ':' {
		return "hash"
	}
	return "unknown"
}
