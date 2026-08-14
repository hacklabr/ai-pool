package routing

import (
	"context"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Porta mínima de auditoria para transições de estado de chaves (issue #19,
// RF-03). O routing NÃO depende do AuditRepo concreto — apenas desta porta;
// o adapter postgres (KeyTransitionAuditorAdapter) faz a ponte. Regra do
// repo: "nova dependência externa = porta mínima no pacote consumidor".
//
// Toda transição (auto-disable, reativação scheduler/lazy, manual, teste)
// gera um evento append-only em audit_log com entity_type='provider_key'.
// O metadata carrega a mensagem de erro upstream SANITIZADA
// (SanitizeUpstreamMessage) e a camada do limite (ClassifyLimitLayer) —
// NUNCA a chave de API ou qualquer segredo.
// =============================================================================

// Ações de auditoria para transições de chave (audit_log.action).
const (
	// AuditActionAutoDisable: enabled → auto_disabled pelo AutoDisableManager.
	AuditActionAutoDisable = "provider_key.auto_disable"
	// AuditActionReactivate: auto_disabled → enabled (scheduler ou lazy).
	AuditActionReactivate = "provider_key.reactivate"
)

// KeyTransitionAuditor é a porta mínima de auditoria de transições de chave.
// Implementações devem ser append-only e best-effort (erro não pode derrubar
// a transição em si).
type KeyTransitionAuditor interface {
	// AppendKeyTransition registra um evento de transição para a chave.
	// metadata pode ser map[string]any ou struct (serializada como JSONB).
	AppendKeyTransition(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID, action string, metadata any) error
}
