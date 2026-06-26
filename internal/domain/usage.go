package domain

import (
	"context"
	"time"
)

// =============================================================================
// Registro de Uso — evento efêmero coletado no path do proxy e persistido de
// forma ASSÍNCRONA (fora do caminho crítico).
//
// Spec §3.2.6:
//   - Logging assíncrono via canal bufferizado + batch insert.
//   - Captura por requisição: quem (user/token), qual chave/provedor/modelo,
//     quantos tokens (prompt/completion/cached), latência, status, quando.
//   - Dead-letter para INSERTs que falham (spec §3.4.2).
//
// UsageEvent é o tipo transitado pelo canal. É definido no DOMÍNIO (não no
// adapter postgres) para que o proxy/routing engine não dependam de detalhes
// de persistência. O adapter postgres converte UsageEvent → UsageLogRecord
// na hora do InsertBatch.
// =============================================================================

// UsageEvent é o evento de uso de UMA requisição ao proxy.
type UsageEvent struct {
	OrganizationID   OrganizationID
	UserID           ID
	TokenID          ID
	ProviderID       ID
	KeyID            ID
	Model            string
	Stream           bool
	PromptTokens     *int // nil se provedor não reportou (stream sem usage)
	CompletionTokens *int
	CachedTokens     *int // alguns provedores reportam (DeepSeek cached_tokens)
	TotalTokens      *int
	LatencyMS        *int
	StatusCode       *int
	ErrorClass       *string // "rate_limit" | "auth" | "timeout" | "5xx" | "quota" | nil
	CreatedAt        time.Time
}

// UsageCollector coleta eventos de uso de forma ASSÍNCRONA.
//
// Implementação concreta (F6): canal bufferizado + goroutine worker que faz
// batch insert. O método Record() é NÃO-BLOQUEANTE — se o canal está cheio,
// o evento é descartado (com log de métrica de drop) para não travar o
// request do usuário.
type UsageCollector interface {
	// Record enfileira o evento para persistência assíncrona. NÃO-BLOQUEANTE.
	// Retorna false se o canal está cheio e o evento foi descartado (metric: drop).
	Record(event UsageEvent) (enqueued bool)

	// Start inicia a goroutine consumidora. Retorna imediatamente; roda até
	// ctx ser cancelado (graceful shutdown).
	Start(ctx context.Context)

	// Close drena o canal pendente e fecha o collector. Chamado em shutdown
	// APÓS o contexto de Start ser cancelado.
	Close() error
}

// NoopUsageCollector é uma implementação que descarta tudo (dev/test sem DB).
type NoopUsageCollector struct{}

func (NoopUsageCollector) Record(_ UsageEvent) bool { return true }
func (NoopUsageCollector) Start(_ context.Context)  {}
func (NoopUsageCollector) Close() error             { return nil }
