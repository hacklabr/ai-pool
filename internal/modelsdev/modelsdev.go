// Package modelsdev consulta o catálogo público https://models.dev/api.json
// (mesma fonte que o OpenCode usa para providers built-in) como fonte de
// especificações de modelos: limites de tokens, modalidades e opções de
// reasoning effort (low/high/max — as "variações" dos modelos).
//
// Substitui o dicionário embutido internal/modelregistry (issue #26,
// ADR-0015) pelo ADR-0017: em vez de manter specs.json versionado no repo,
// lemos a fonte canonical que o próprio ecossistema OpenCode mantém —
// cobertura de novos modelos sem PR no ai-pool.
//
// Princípios herdados do modelsync (ADR-0015):
//   - FAIL-SAFE: fetch/parse falho → erro retornado ao caller, que decide
//     seguir sem enriquecimento (modelsync já trata como warn, sem alterar
//     o catálogo local).
//   - Lookup GLOBAL por model-id, sem mapeamento por provider no banco: ids
//     de provedores oficiais (zai*, kimi*, moonshotai*) têm PRIORIDADE sobre
//     agregadores (openrouter, nano-gpt etc.) que também listam o mesmo id
//     (às vezes com limites diferentes).
//
// O pacote é uma folha (apenas stdlib), sem dependência de domain/infra.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultURL é o endpoint canonical do catálogo (público, sem auth).
const DefaultURL = "https://models.dev/api.json"

// maxResponseBytes limita o body do catálogo (defesa DoS; atual ~4MB, margem).
const maxResponseBytes = 16 * 1024 * 1024

// defaultTimeout limita o fetch do catálogo inteiro.
const defaultTimeout = 30 * time.Second

// Spec é a especificação de um modelo resolvida por id global.
type Spec struct {
	ID              string
	ContextWindow   int
	OutputLimit     int
	Modalities      []string // união input+output (ex.: ["text","image","video"])
	ReasoningEffort []string // valores de effort (ex.: ["low","high","max"]); vazio = sem seletor
}

// officialPrefixes são os providers de primeira parte no models.dev: quando
// o mesmo model-id aparece em vários providers, o oficial vence (agregadores
// podem reportar limites distintos ou desatualizados).
var officialPrefixes = []string{"zai", "kimi", "moonshotai"}

// Client busca e indexa o catálogo models.dev. Uma instância é construída
// por rodada de sync (Fetch) e fica imutável depois — concorrência segura
// para leitura.
type Client struct {
	httpClient *http.Client
	url        string
}

// NewClient cria o cliente. httpClient nil → client padrão com timeout do
// fetch (aplicado por request, não no construtor, para permitir reuso).
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{httpClient: httpClient, url: DefaultURL}
}

// SpecIndex é o índice id→Spec construído por um fetch; imutável pós-load.
type SpecIndex struct {
	byID map[string]Spec
}

// modelEntry espelha o schema models.dev (campos relevantes apenas; o parse
// é tolerante — campos extras são ignorados).
type modelEntry struct {
	Name             string          `json:"name"`
	Limit            *limitEntry     `json:"limit"`
	Modalities       *modalitiesExpr `json:"modalities"`
	Modality         *modalityLegacy `json:"modality"`
	Reasoning        bool            `json:"reasoning"`
	ReasoningOptions []reasoningOpt  `json:"reasoning_options"`
}

type limitEntry struct {
	Context *int `json:"context"`
	Output  *int `json:"output"`
}

type modalitiesExpr struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// modalityLegacy cobre o campo antigo "modality" (input/output) ainda
// presente em parte do catálogo.
type modalityLegacy struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type reasoningOpt struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type providerEntry struct {
	Models map[string]modelEntry `json:"models"`
}

type catalogFile struct {
	Providers map[string]providerEntry `json:"-"`
}

// Fetch baixa o catálogo e constrói o índice. O parse é tolerante:
// providers sem models e entradas sem id/limit são ignorados silenciosamente
// (o catálogo é grande e parcialmente comunitário).
func (c *Client) Fetch(ctx context.Context) (*SpecIndex, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: invalid url: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("modelsdev: upstream status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("modelsdev: read body: %w", err)
	}
	return parseCatalog(body)
}

// parseCatalog valida o JSON e aplica a precedência oficial→agregador.
// Exposto como função interna para testes com fixture embutida.
func parseCatalog(body []byte) (*SpecIndex, error) {
	var raw map[string]providerEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("modelsdev: parse catalog: %w", err)
	}

	idx := &SpecIndex{byID: make(map[string]Spec, 1024)}
	// Ordem determinística: providers oficiais primeiro (prefixo), depois o
	// resto em ordem alfabética — dentro do mesmo grupo, o PRIMEIRO vence.
	order := make([]string, 0, len(raw))
	for slug := range raw {
		order = append(order, slug)
	}
	sort.Slice(order, func(i, j int) bool {
		oi, oj := officialRank(order[i]), officialRank(order[j])
		if oi != oj {
			return oi < oj
		}
		return order[i] < order[j]
	})

	for _, slug := range order {
		prov := raw[slug]
		for id, m := range prov.Models {
			if id == "" {
				continue
			}
			if _, exists := idx.byID[id]; exists {
				continue // primeiro vence (oficial > agregador)
			}
			spec, ok := m.toSpec(id)
			if !ok {
				continue
			}
			idx.byID[id] = spec
		}
	}
	return idx, nil
}

// officialRank: 0 para provedor oficial (por prefixo), 1 para agregador.
func officialRank(slug string) int {
	for _, p := range officialPrefixes {
		if slug == p || strings.HasPrefix(slug, p+"-") {
			return 0
		}
	}
	return 1
}

// toSpec converte a entrada do catálogo; ok=false quando não há limites
// utilizáveis (sem context/output o enriquecimento não tem o que oferecer
// — modelos assim ficam a cargo dos defaults de schema do modelcatalog).
func (m *modelEntry) toSpec(id string) (Spec, bool) {
	spec := Spec{ID: id}
	if m.Limit != nil {
		if m.Limit.Context != nil && *m.Limit.Context > 0 {
			spec.ContextWindow = *m.Limit.Context
		}
		if m.Limit.Output != nil && *m.Limit.Output > 0 {
			spec.OutputLimit = *m.Limit.Output
		}
	}
	if spec.ContextWindow == 0 || spec.OutputLimit == 0 {
		return Spec{}, false
	}

	spec.Modalities = m.modalities()
	spec.ReasoningEffort = m.effortValues()
	return spec, true
}

// modalidades normaliza os dois formatos do catálogo: "modalities"
// (input/output separados) e o legado "modality". União ordenada sem dups.
func (m *modelEntry) modalities() []string {
	var in, out []string
	if m.Modalities != nil {
		in, out = m.Modalities.Input, m.Modalities.Output
	} else if m.Modality != nil {
		in, out = m.Modality.Input, m.Modality.Output
	}
	seen := make(map[string]bool, len(in)+len(out))
	var mods []string
	for _, v := range append(append([]string(nil), in...), out...) {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		mods = append(mods, v)
	}
	return mods
}

// effortValues extrai os valores de reasoning do tipo "effort"
// (ex.: ["low","high","max"]). Toggle puro não gera variantes — sem
// valores conhecidos o seletor do OpenCode não tem o que alternar.
func (m *modelEntry) effortValues() []string {
	for _, ro := range m.ReasoningOptions {
		if ro.Type == "effort" && len(ro.Values) > 0 {
			out := make([]string, 0, len(ro.Values))
			for _, v := range ro.Values {
				if v != "" {
					out = append(out, v)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// FetchStub constrói um índice a partir de specs prontas SEM rede — para
// testes de consumidores (modelsync) e para ferramentas offline.
func FetchStub(specs map[string]Spec) (*SpecIndex, error) {
	idx := &SpecIndex{byID: make(map[string]Spec, len(specs))}
	for id, s := range specs {
		if id == "" {
			return nil, fmt.Errorf("modelsdev: stub spec with empty id")
		}
		if s.ContextWindow <= 0 || s.OutputLimit <= 0 {
			return nil, fmt.Errorf("modelsdev: stub %q has non-positive limits (%d/%d)",
				id, s.ContextWindow, s.OutputLimit)
		}
		s.ID = id
		idx.byID[id] = s
	}
	return idx, nil
}

// Lookup retorna a spec para o id exato. O slice de retorno é cópia
// defensiva — mutar o retorno não altera o índice.
func (i *SpecIndex) Lookup(id string) (Spec, bool) {
	spec, ok := i.byID[id]
	if !ok {
		return Spec{}, false
	}
	spec.Modalities = append([]string(nil), spec.Modalities...)
	spec.ReasoningEffort = append([]string(nil), spec.ReasoningEffort...)
	return spec, true
}

// Len retorna o número de modelos indexados (telemetria/logs de sync).
func (i *SpecIndex) Len() int { return len(i.byID) }
