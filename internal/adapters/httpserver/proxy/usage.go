package proxy

import (
	"strconv"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/observability"
)

// =============================================================================
// Helpers de registro de uso (F6).
//
// Centraliza a construção de UsageEvent a partir do contexto do request.
// O método recordUsage no Server chama o collector se configurado.
// =============================================================================

// recordUsage constrói e enfileira um UsageEvent no collector.
//
// Se o collector é nil (F3/F4 sem F6 configurado), é no-op.
//
// Parâmetros:
//   - user, tok: identidades do request.
//   - spec: parse do body (model, stream).
//   - prompt/completion/cached: tokens (0 se não reportado).
//   - statusCode: HTTP status final enviado ao cliente.
//   - errorClass: classificação ("rate_limit"/"auth"/"timeout"/"5xx"/"quota"/nil).
//   - latencyMS: duração do forward.
//   - providerSlug: identificação do provider (para auditoria).
func (s *Server) recordUsage(
	user *domain.User,
	tok *domain.APIToken,
	spec upstreamRequestSpec,
	promptTokens, completionTokens, cachedTokens, totalTokens int,
	statusCode int,
	errorClass *string,
	latencyMS int64,
	providerSlug string,
) {
	if s.usageCollector == nil || user == nil {
		return
	}
	sc := statusCode
	st := int(latencyMS)
	pt := promptTokens
	ct := completionTokens
	ch := cachedTokens
	tt := totalTokens
	ev := domain.UsageEvent{
		OrganizationID:   user.OrgID,
		UserID:           user.ID,
		Model:            spec.Model,
		Stream:           spec.IsStream,
		PromptTokens:     &pt,
		CompletionTokens: &ct,
		CachedTokens:     &ch,
		TotalTokens:      &tt,
		StatusCode:       &sc,
		LatencyMS:        &st,
		ErrorClass:       errorClass,
		CreatedAt:        time.Now().UTC(),
	}
	if tok != nil {
		ev.TokenID = tok.ID
	}
	// ProviderID e KeyID são 0 quando não resolvido (ex.: rate-limit antes do forward).
	// Em produção (failover wrapper), o wrapper preenche estes campos antes de Record.
	s.usageCollector.Record(ev)

	// === Instrumentação Prometheus (F11) ===
	// Métricas sempre inc'd (mesmo com /metrics endpoint desabilitado). O custo
	// de um Counter.Inc() é ~100ns; ignorável no hot path.
	observability.IncProxyRequest(providerSlug, spec.Model, statusCode)
	observability.AddTokens(providerSlug, spec.Model, "prompt", promptTokens)
	observability.AddTokens(providerSlug, spec.Model, "completion", completionTokens)
	observability.AddTokens(providerSlug, spec.Model, "cached", cachedTokens)
}

// ptrString converte string para *string.
func ptrString(s string) *string { return &s }

// RecordProxyUsage é a forma EXPORTADA de registro de uso, usada pelo wrapper
// failover (pacote failover) para registrar o resultado FINAL enviado ao
// cliente. O chain chama HandleResponse no máximo uma vez (só na tentativa que
// produz a resposta ao cliente), então não há dupla contagem.
//
// Diferente de recordUsage, recebe providerID/keyID do KeyResolution (para
// alimentar os rollups por provedor no dashboard). tok é omitido (nil) — o
// agrupamento por user já basta para "Meu uso".
func (s *Server) RecordProxyUsage(
	user *domain.User,
	spec upstreamRequestSpec,
	promptTokens, completionTokens, cachedTokens, statusCode int,
	latencyMS int64,
	errorClass *string,
	providerSlug string,
	providerID, keyID int64,
) {
	if s.usageCollector == nil || user == nil {
		return
	}
	sc := statusCode
	st := int(latencyMS)
	pt := promptTokens
	ct := completionTokens
	ch := cachedTokens
	tt := promptTokens + completionTokens
	s.usageCollector.Record(domain.UsageEvent{
		OrganizationID:   user.OrgID,
		UserID:           user.ID,
		Model:            spec.Model,
		Stream:           spec.IsStream,
		PromptTokens:     &pt,
		CompletionTokens: &ct,
		CachedTokens:     &ch,
		TotalTokens:      &tt,
		StatusCode:       &sc,
		LatencyMS:        &st,
		ErrorClass:       errorClass,
		CreatedAt:        time.Now().UTC(),
		ProviderID:       domain.ID(providerID),
		KeyID:            domain.ID(keyID),
	})
	observability.IncProxyRequest(providerSlug, spec.Model, statusCode)
	observability.AddTokens(providerSlug, spec.Model, "prompt", promptTokens)
	observability.AddTokens(providerSlug, spec.Model, "completion", completionTokens)
	observability.AddTokens(providerSlug, spec.Model, "cached", cachedTokens)
}

// ptrInt converte int para *int.
func ptrInt(i int) *int { return &i }

// formatLatencyMS helper para logging (não usado em recordUsage).
func formatLatencyMS(d time.Duration) int64 {
	return d.Milliseconds()
}

// intToStr helper trivial.
func intToStr(i int64) string { return strconv.FormatInt(i, 10) }
