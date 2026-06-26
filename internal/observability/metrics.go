// Package observability centraliza a instrumentação Prometheus do AI Provider Pool.
//
// Métricas definidas (spec §3.5.4 — habilitadas para F11, mas endpoint /metrics
// GATEADO por config.EnableMetrics; instrumentation sempre ativa no binário):
//
//   - aipp_proxy_requests_total{provider,model,status}
//     Conta requisições que passam pelo proxy /v1/*. Inclui 4xx/5xx.
//
//   - aipp_proxy_request_duration_seconds{provider,model}
//     Histograma de latência total da requisição (do recebimento ao último byte).
//     Buckets otimizadas para latências típicas de LLM (50ms a 60s).
//
//   - aipp_failover_attempts_total{provider}
//     Conta CADA tentativa ao upstream (1 request pode gerar N tentativas se houver
//     failover). Útil para calcular taxa de failover = (attempts - requests)/requests.
//
//   - aipp_tokens_total{provider,model,type=prompt|completion|cached}
//     Conta tokens processados por request. Type "cached" mede economia de cache
//     de contexto (diferencial do produto).
//
//   - aipp_keys_disabled_total{reason=auto|manual}
//     Conta desabilitações de chave. Auto = disparado pelo AutoDisableManager;
//     manual = admin via API.
//
//   - aipp_key_reactivations_total
//     Conta reativações pelo scheduler (chaves cujo disabled_until expirou).
//
// Filosofia (DevOps T1 §7.3):
//   - Instrumentação sempre presente (custo baixo, pavimenta ROADMAP).
//   - Endpoint /metrics GATEADO por env (default false no MVP). Quando chegar
//     Prometheus + Grafana no cluster, basta ligar ENABLE_METRICS=true.
//   - Nomes prefixados com `aipp_` para evitar colisão com Go runtime metrics.
//   - Labels usadas com moderação (alta cardinalidade = memória Prometheus).
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// =============================================================================
// Métricas — declaradas UMA VEZ via package-level vars (com promauto para
// registro automático no DefaultRegisterer).
//
// Para mudar o registry (ex.: em testes), use prometheus.NewRegistry() e
// recrie as métricas. No app prod, o default está OK.
// =============================================================================

// proxyRequestsTotal conta requisições que chegam ao handler /v1/*.
// Labels: provider (slug do provedor), model (nome do modelo), status (HTTP).
var proxyRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aipp_proxy_requests_total",
		Help: "Total number of proxy requests handled by /v1/* (includes 4xx/5xx).",
	},
	[]string{"provider", "model", "status"},
)

// proxyRequestDurationSeconds histograma de latência total.
// Buckets otimizadas para LLM (first-byte rápido, overall pode chegar a minutos).
var proxyRequestDurationSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "aipp_proxy_request_duration_seconds",
		Help:    "Total request duration (from received to last byte sent).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	},
	[]string{"provider", "model"},
)

// failoverAttemptsTotal conta cada tentativa ao upstream.
// 1 request com failover = 2+ tentativas.
var failoverAttemptsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aipp_failover_attempts_total",
		Help: "Total upstream attempts (failover adds extra attempts per request).",
	},
	[]string{"provider"},
)

// tokensTotal conta tokens processados.
// Label type: prompt|completion|cached (cached mede economia de cache de contexto).
var tokensTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aipp_tokens_total",
		Help: "Total tokens processed by the proxy (prompt/completion/cached).",
	},
	[]string{"provider", "model", "type"},
)

// keysDisabledTotal conta desabilitações de chave (auto vs manual).
var keysDisabledTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "aipp_keys_disabled_total",
		Help: "Total provider keys disabled (auto=AutoDisableManager, manual=admin API).",
	},
	[]string{"reason"},
)

// keyReactivationsTotal contador absoluto de reativações pelo scheduler.
var keyReactivationsTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "aipp_key_reactivations_total",
		Help: "Total provider keys reactivated by the scheduler (disabled_until expired).",
	},
)

// =============================================================================
// Helpers públicos — ponto ÚNICO de uso para cada métrica.
//
// Estes helpers existem para:
//   - Centralizar a API (fácil de grep "onde a métrica é inc'd").
//   - Garantir consistência de labels (evita divergência de cardinalidade).
//   - Permitir no-op fácil em testes (substituir o helper por mock).
// =============================================================================

// IncProxyRequest incrementa o contador de requests. Chamado no fim do handleV1.
//
// provider pode ser "" se não resolvido (ex.: erro antes do forward).
// model é o spec.Model do body (também pode ser "" em endpoints sem model).
// status é o HTTP status code retornado ao cliente.
func IncProxyRequest(provider, model string, status int) {
	proxyRequestsTotal.WithLabelValues(sanitizeLabel(provider), sanitizeLabel(model), statusLabel(status)).Inc()
}

// ObserveProxyDuration registra a duração total de uma request.
// provider/model mesmos de IncProxyRequest.
func ObserveProxyDuration(provider, model string, seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	proxyRequestDurationSeconds.WithLabelValues(sanitizeLabel(provider), sanitizeLabel(model)).Observe(seconds)
}

// IncFailoverAttempt é chamado uma vez por TENTATIVA ao upstream.
// Importante: NÃO é "uma por request" — um request com failover gera 2+ incs.
// provider pode ser "" se o chain ainda não sabe (será "unknown" no painel).
func IncFailoverAttempt(provider string) {
	failoverAttemptsTotal.WithLabelValues(sanitizeLabel(provider)).Inc()
}

// AddTokens adiciona tokens ao contador apropriado.
// tokenType: "prompt" | "completion" | "cached".
// n DEVE ser >= 0 (0 é no-op implícito do prometheus, mas explícito aqui).
func AddTokens(provider, model, tokenType string, n int) {
	if n <= 0 {
		return
	}
	tokensTotal.WithLabelValues(sanitizeLabel(provider), sanitizeLabel(model), sanitizeTokenType(tokenType)).Add(float64(n))
}

// IncKeyDisabled incrementa contador de desabilitação.
// reason: "auto" (AutoDisableManager) ou "manual" (admin API force-disable).
func IncKeyDisabled(reason string) {
	keysDisabledTotal.WithLabelValues(sanitizeDisableReason(reason)).Inc()
}

// IncKeyReactivation registra uma reativação pelo scheduler.
func IncKeyReactivation() {
	keyReactivationsTotal.Inc()
}

// =============================================================================
// Sanitizers de labels — evitam cardinalidade explosiva por erros de digitação
// ou valores inválidos vindo de callers.
// =============================================================================

// sanitizeLabel substitui string vazia por "unknown" para evitar label value
// vazia (que é permitida pelo Prometheus mas confusa em painéis).
func sanitizeLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// statusLabel formata status HTTP como string ("200", "404", etc.) — o Prometheus
// espera string para label values.
func statusLabel(s int) string {
	// Fast path para status comuns (evita alloc do Itoa em hot path).
	switch s {
	case 200:
		return "200"
	case 400:
		return "400"
	case 401:
		return "401"
	case 403:
		return "403"
	case 404:
		return "404"
	case 429:
		return "429"
	case 500:
		return "500"
	case 502:
		return "502"
	case 503:
		return "503"
	default:
		// Fallback para status menos comuns.
		return itoa(s)
	}
}

// sanitizeTokenType valida que tokenType é um dos valores conhecidos.
// Evita criar label "type=invalid_kind" por erro de caller.
func sanitizeTokenType(t string) string {
	switch t {
	case "prompt", "completion", "cached":
		return t
	default:
		return "unknown"
	}
}

// sanitizeDisableReason valida reason para keys_disabled_total.
func sanitizeDisableReason(r string) string {
	switch r {
	case "auto", "manual":
		return r
	default:
		return "unknown"
	}
}

// itoa converte int → string sem importar strconv (mantém o pacote enxuto).
// Usado apenas no fallback de statusLabel.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte // int máximo ~10 dígitos + sinal
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
