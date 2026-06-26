package domain

import (
	"context"
	"time"
)

// =============================================================================
// Routing Engine — PORTAS estendidas para F5.
//
// A interface KeyResolver (keyresolver.go) é mantida para uso genérico. Para
// o routing engine completo (LRU + afinidade + failover), usamos a interface
// estendida RoutingEngine, que adiciona:
//   - ResolveKeyWithAffinity: aceita userID, conversationID, prefixHash e lista
//     de chaves a excluir (já tentadas no failover chain).
//   - OnKeySucceeded: callback após uso bem-sucedido (atualiza afinidade).
//   - OnKeyFailed: callback após erro classificado (auto-disable + stores).
//
// O FailoverChain (orquestrador stateful) usa estas portas; o proxy.Server
// delega para o FailoverChain ao invés de chamar KeyResolver diretamente.
// =============================================================================

// RoutingEngine estende KeyResolver com capacidades de routing completo.
//
// Implementa KeyResolver (método ResolveKey) para compat — delega para
// ResolveKeyWithAffinity com defaults (sem userID/convID/exclude).
type RoutingEngine interface {
	KeyResolver

	// ResolveKeyWithAffinity escolhe uma chave considerando afinidade de cache
	// de contexto (X-Conversation-ID ou hash do prefixo) e excluindo chaves
	// já tentadas no failover chain desta requisição.
	//
	// Parâmetros:
	//   - orgID:           organização do usuário (multi-tenant ready).
	//   - userID:          usuário autenticado (afinidade é POR usuário).
	//   - model:           nome do modelo solicitado.
	//   - conversationKey: chave de conversa derivada (X-Conversation-ID ou hash
	//                      do prefixo). Vazio se não há continuidade estabelecida.
	//   - exclude:         keyIDs já tentadas nesta requisição (failover).
	//
	// Retorna *KeyResolution ou ErrNoKeysAvailable/ErrModelNotFound.
	ResolveKeyWithAffinity(
		ctx context.Context,
		orgID OrganizationID,
		userID ID,
		model string,
		conversationKey string,
		exclude []ID,
	) (*KeyResolution, error)

	// OnKeySucceeded é chamado após a chave atender com sucesso (status 2xx
	// completo). Atualiza:
	//   - LRUStore.MarkUsed(keyID) — promove para "recentemente usada".
	//   - AffinityStore.Remember(userID, conversationKey, keyID, messageCount)
	//     — mapeia conversa→chave para próximas requisições.
	//   - Reset do contador de erros consecutivos (AutoDisableManager).
	OnKeySucceeded(ctx context.Context, orgID OrganizationID, userID ID, conversationKey string, keyID ID, messageCount int) error

	// OnKeyFailed é chamado após erro classificado como failover (401/403/429/
	// 5xx/network). Registra erro no AutoDisableManager (que pode desabilitar
	// a chave se atingir threshold).
	//
	// providerSlug é usado para selecionar o ErrorParser apropriado no registry.
	OnKeyFailed(ctx context.Context, orgID OrganizationID, keyID ID, providerID ID, providerSlug string, statusCode int, errBody []byte) error
}

// =============================================================================
// ErrorParser — porta para parsers de mensagem de erro por provider.
// =============================================================================

// ErrorParser extrai data de liberação de cota de mensagens de erro.
//
// Cada provedor tem formato próprio (ISO 8601, "in N seconds", unix epoch).
// Implementações específicas se registram no ParserRegistry por slug.
type ErrorParser interface {
	// ParseResetDate analisa statusCode + body e retorna:
	//   - (data, true):  data de liberação detectada.
	//   - (zero, false): parsing não achou data (caller usa backoff crescente).
	ParseResetDate(statusCode int, body []byte) (time.Time, bool)
}

// =============================================================================
// Erros adicionais do routing.
// =============================================================================

// ErrKeyDisabled é retornado quando uma chave foi desabilitada (auto/manual)
// e não pode ser usada. Usado internamente pelo engine para detectar chaves
// em afinidade que saíram do pool.
var ErrKeyDisabled = &sentinelError{"key is disabled"}

// ErrRoutingInternal erro interno do routing engine (DB, decrypt, etc.).
// Não deve ser exposto ao cliente; o proxy converte em 500/502.
var ErrRoutingInternal = &sentinelError{"routing engine internal error"}
