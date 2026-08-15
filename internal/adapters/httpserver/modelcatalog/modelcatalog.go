// Package modelcatalog converte o catálogo local de modelos (domain.Model)
// para os formatos consumidos pelos clientes do pool.
//
// Extraído de admin/invite.go (activeModels) para ser compartilhado entre o
// aceite de convite (admin) e o endpoint agregado GET /v1/models (proxy,
// variante ?format=opencode) sem import cycle e sem poluir o domain (RF-12,
// RF-13, ADR-0015).
package modelcatalog

import (
	"github.com/hacklabr/ai-pool/internal/domain"
)

// OpenCodeModelsMap converte modelos para o schema esperado pelo OpenCode
// (provider.{id}.models.{model}):
//
//	{ "<model-name>": { "name": <display>, "modalities"?: {...}, "limit": {...} } }
//
// Apenas modelos ativos entram no mapa. Modelos multimodais incluem o bloco
// "modalities" no formato esperado pelo @ai-sdk/openai-compatible.
//
// O bloco "limit" é SEMPRE emitido com "context" E "output" (issue #29): o
// OpenCode valida limit.output como obrigatório em todo modelo do provider
// openai-compatible — omiti-lo produz uma config inválida que impede o
// OpenCode de iniciar. Quando o catálogo não conhece o valor (campo nulo),
// aplicamos DEFAULTS CONSERVADORES para garantir validade — são fallbacks de
// schema, não verdades sobre o modelo:
//   - context: DefaultContextWindow (128K)
//   - output:  DefaultOutputLimit (32K)
//
// Chave do mapa = Model.Name. Se houver nomes repetidos na entrada, a ÚLTIMA
// ocorrência vence — callers que precisam de precedência (ex.: "primeiro
// provider ativo vence") devem pré-filtrar/deduplicar a lista.

// DefaultContextWindow é o fallback de schema quando o catálogo não conhece
// a janela de contexto do modelo (128K — típico de modelos atuais).
const DefaultContextWindow = 131072

// DefaultOutputLimit é o fallback de schema quando o catálogo não conhece o
// limite de saída (32K — piso seguro; exceder o real truncaria respostas,
// ficar abaixo só limita o máximo por resposta).
const DefaultOutputLimit = 32768

func OpenCodeModelsMap(models []domain.Model) map[string]any {
	out := make(map[string]any, len(models))
	for _, m := range models {
		if !m.IsActive {
			continue
		}
		display := m.Name
		if m.DisplayName != nil && *m.DisplayName != "" {
			display = *m.DisplayName
		}
		model := map[string]any{"name": display}
		if m.IsMultimodal() {
			model["modalities"] = map[string]any{
				"input":  []string{"text", "image"},
				"output": []string{"text"},
			}
		}
		// Bloco "limit" COMPLETO e sempre presente (issue #29): o OpenCode
		// exige limit.output — config sem ele é rejeitada no startup.
		context := DefaultContextWindow
		if m.ContextWindow != nil && *m.ContextWindow > 0 {
			context = *m.ContextWindow
		}
		output := DefaultOutputLimit
		if m.OutputLimit != nil && *m.OutputLimit > 0 {
			output = *m.OutputLimit
		}
		model["limit"] = map[string]any{"context": context, "output": output}
		out[m.Name] = model
	}
	return out
}

// Entry é um modelo ativo de um provider ativo, pronto para serialização no
// formato OpenAI de listagem (GET /v1/models).
type Entry struct {
	Name         string
	ProviderSlug string
	CreatedUnix  int64
}

// OpenAIList serializa entradas no formato OpenAI:
//
//	{"object":"list","data":[{"id":...,"object":"model","created":...,"owned_by":...}]}
//
// data é sempre um array (possivelmente vazio), nunca null.
func OpenAIList(entries []Entry) map[string]any {
	data := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		data = append(data, map[string]any{
			"id":       e.Name,
			"object":   "model",
			"created":  e.CreatedUnix,
			"owned_by": e.ProviderSlug,
		})
	}
	return map[string]any{"object": "list", "data": data}
}
