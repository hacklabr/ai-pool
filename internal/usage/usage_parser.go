package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// =============================================================================
// Parse de tokens do response do provedor.
//
// Spec §3.2.6:
//   - Non-streaming: campo "usage" no JSON final do response.
//   - Streaming SSE: o último chunk contém "usage" (alguns provedores).
//   - Campos: prompt_tokens, completion_tokens, total_tokens. Opcionais:
//     cached_tokens, prompt_tokens_details.cached_tokens (DeepSeek/OpenAI novo).
//
// Quando o provedor NÃO reporta usage (stream sem include_usage), todos os
// campos ficam nil — o usage_log registra NULL (não estimamos no MVP).
// =============================================================================

// UsageStats contém os tokens extraídos do response do provedor.
// Todos os campos são *int (nil se não reportado).
type UsageStats struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	// CachedTokens: alguns provedores reportam no nível raiz; outros em
	// prompt_tokens_details.cached_tokens. Capturamos ambos.
	CachedTokens *int `json:"cached_tokens"`
}

// usageStatsFlex captura variantes de schema entre provedores.
type usageStatsFlex struct {
	PromptTokens     *int           `json:"prompt_tokens"`
	CompletionTokens *int           `json:"completion_tokens"`
	TotalTokens      *int           `json:"total_tokens"`
	CachedTokens     *int           `json:"cached_tokens"`         // nível raiz (DeepSeek)
	PromptDetails    *promptDetails `json:"prompt_tokens_details"` // OpenAI novo
}

type promptDetails struct {
	CachedTokens *int `json:"cached_tokens"`
}

// ParseUsageFromBody extrai UsageStats de um body JSON NÃO-STREAMING.
//
// Body deve ter o formato OpenAI-compatible:
//
//	{"id":"...","usage":{"prompt_tokens":10,"completion_tokens":20,...}}
//
// Retorna (zero, false) se body não contém campo "usage" ou é inválido.
func ParseUsageFromBody(body []byte) (UsageStats, bool) {
	if len(body) == 0 {
		return UsageStats{}, false
	}
	var wrapper struct {
		Usage *usageStatsFlex `json:"usage"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return UsageStats{}, false
	}
	if wrapper.Usage == nil {
		return UsageStats{}, false
	}
	stats := UsageStats{
		PromptTokens:     wrapper.Usage.PromptTokens,
		CompletionTokens: wrapper.Usage.CompletionTokens,
		TotalTokens:      wrapper.Usage.TotalTokens,
		CachedTokens:     wrapper.Usage.CachedTokens,
	}
	// Fallback: cached_tokens em prompt_tokens_details (OpenAI novo formato).
	if stats.CachedTokens == nil && wrapper.Usage.PromptDetails != nil {
		stats.CachedTokens = wrapper.Usage.PromptDetails.CachedTokens
	}
	// Tem uso se pelo menos um campo não-nil.
	if stats.PromptTokens == nil && stats.CompletionTokens == nil && stats.TotalTokens == nil {
		return UsageStats{}, false
	}
	return stats, true
}

// ParseUsageFromStreamBody extrai UsageStats lendo um body SSE já consumido
// (io.Reader dos bytes brutos do stream).
//
// Em SSE, o usage aparece no ÚLTIMO chunk antes de [DONE]:
//
//	data: {"choices":[...],"usage":{"prompt_tokens":10,...,"total_tokens":30}}
//	data: [DONE]
//
// Estratégia: escaneia TODAS as linhas "data: {...}", faz parse JSON de cada
// uma, e mantém o ÚLTIMO usage encontrado (que é o agregado final).
//
// NÃO consome o reader de forma streaming (já foi consumido e enviado ao
// cliente pelo proxy). Espera um buffer com TODO o conteúdo do stream.
//
// Retorna (zero, false) se nenhum chunk continha usage.
func ParseUsageFromStreamBody(body io.Reader) (UsageStats, bool) {
	scanner := bufio.NewScanner(body)
	// Aumenta buffer para suportar chunks grandes (default 64KB pode estourar).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lastStats UsageStats
	found := false

	for scanner.Scan() {
		line := scanner.Text()
		// Linhas SSE começam com "data: ".
		if !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "data:") {
			continue
		}
		// Remove prefixo "data: ".
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
		if data == "[DONE]" || data == "" {
			continue
		}
		// Tenta parsear como JSON e extrair usage.
		if stats, ok := ParseUsageFromBody([]byte(data)); ok {
			lastStats = stats
			found = true
		}
	}
	return lastStats, found
}

// ParseUsageFromStreamBytes é conveniência para byte slices.
func ParseUsageFromStreamBytes(body []byte) (UsageStats, bool) {
	return ParseUsageFromStreamBody(strings.NewReader(string(body)))
}
