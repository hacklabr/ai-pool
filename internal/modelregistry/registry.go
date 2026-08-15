// Package modelregistry mantém o dicionário EMBUTIDO de especificações de
// modelos de LLM (issue #26, complemento da RF-11/ADR-0015).
//
// O endpoint de listagem dos provedores (GET /models) só traz os ids — os
// modelos nascem no catálogo local sem context_window/output_limit/modalities.
// Este pacote fornece um lookup nome→spec versionado no repo (specs.json,
// embutido via embed.FS, zero dependências novas) para o modelsync:
//   - modelo NOVO presente no dicionário nasce já configurado;
//   - modelo EXISTENTE com campo vazio recebe backfill.
//
// REGRA INVARIANTE (ADR-0015): o dicionário só preenche campo VAZIO (nulo);
// NUNCA sobrescreve valor existente (manual ou de sync anterior). A aplicação
// fica a cargo do consumidor (modelsync + ModelRepo.BackfillSpecs).
//
// Formato do specs.json: entrada por nome canônico; "aliases" são nomes
// alternativos que o provedor lista para o MESMO modelo, herdando os campos
// do pai exceto onde sobrescrevem (ex.: mesma família com contexto menor).
// Entrada sem aliases = só o nome exato bate. Lookup é case-sensitive
// (ids de modelo são case-sensitive nos provedores).
//
// Fontes dos dados validados (2026-08-14, ambiente de teste):
//   - Z.ai (docs.z.ai, páginas por modelo): glm-4.5 .. glm-5.3
//   - Kimi (platform.kimi.ai): k3-quickstart (max_completion_tokens up to
//     1048576, default 131072), k2.7-code-quickstart (max_tokens default
//     32768), models-overview (context windows)
//
// O pacote é uma folha (apenas stdlib), sem dependência de domain/infra —
// pode ser consumido por qualquer camada.
package modelregistry

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
)

//go:embed specs.json
var specsFS embed.FS

// Spec é a especificação de um modelo, resolvida por nome canônico OU alias.
// Todos os campos são sempre conhecidos: aliases herdam os valores do pai
// exceto onde sobrescrevem, e a validação no load exige valores positivos.
// Canonical preserva o nome do modelo pai mesmo quando a consulta foi por
// alias (útil para telemetria/auditoria do enriquecimento).
type Spec struct {
	Canonical     string
	ContextWindow int
	OutputLimit   int
	Modalities    []string
}

// modelEntry é a forma serializada de uma entrada do specs.json.
type modelEntry struct {
	Canonical     string       `json:"canonical"`
	ContextWindow int          `json:"context_window"`
	OutputLimit   int          `json:"output_limit"`
	Modalities    []string     `json:"modalities"`
	Aliases       []aliasEntry `json:"aliases,omitempty"`
}

// aliasEntry é um nome alternativo; campos ausentes herdam do pai.
type aliasEntry struct {
	Name          string   `json:"name"`
	ContextWindow *int     `json:"context_window,omitempty"`
	OutputLimit   *int     `json:"output_limit,omitempty"`
	Modalities    []string `json:"modalities,omitempty"`
}

type registryFile struct {
	Models []modelEntry `json:"models"`
}

// Registry é o índice nome→Spec, construído UMA vez no load e imutável
// depois. Concorrência segura para leitura (nenhuma mutação pós-load).
type Registry struct {
	byName map[string]Spec
}

// Default carrega o dicionário embutido (specs.json via embed.FS).
// Erro aqui é estrutural (binário corrompido/JSON inválido) e o startup
// deve tratar como fail-fast.
func Default() (*Registry, error) {
	return Load(specsFS, "specs.json")
}

// Load carrega e valida o dicionário de um fs.FS (exposto para testes com
// fstest.MapFS). Falha rápido em: JSON inválido, canonical/alias vazio,
// valores não-positivos, modalidades vazias ou nomes duplicados
// (canônico×canônico, alias×alias, alias×canônico) — ambiguidade no lookup
// é bug de conteúdo e não pode passar silenciosa.
func Load(fsys fs.FS, path string) (*Registry, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("modelregistry: read %s: %w", path, err)
	}
	var file registryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("modelregistry: parse %s: %w", path, err)
	}

	r := &Registry{byName: make(map[string]Spec, 2*len(file.Models))}
	for _, m := range file.Models {
		if m.Canonical == "" {
			return nil, fmt.Errorf("modelregistry: entry with empty canonical name")
		}
		if m.ContextWindow <= 0 || m.OutputLimit <= 0 {
			return nil, fmt.Errorf("modelregistry: %q has non-positive window/limit (%d/%d)",
				m.Canonical, m.ContextWindow, m.OutputLimit)
		}
		if len(m.Modalities) == 0 {
			return nil, fmt.Errorf("modelregistry: %q has empty modalities", m.Canonical)
		}
		base := Spec{
			Canonical:     m.Canonical,
			ContextWindow: m.ContextWindow,
			OutputLimit:   m.OutputLimit,
			Modalities:    m.Modalities,
		}
		if err := r.add(m.Canonical, base); err != nil {
			return nil, err
		}
		for _, a := range m.Aliases {
			if a.Name == "" {
				return nil, fmt.Errorf("modelregistry: alias of %q has empty name", m.Canonical)
			}
			if a.ContextWindow != nil && *a.ContextWindow <= 0 {
				return nil, fmt.Errorf("modelregistry: alias %q of %q has non-positive context_window", a.Name, m.Canonical)
			}
			if a.OutputLimit != nil && *a.OutputLimit <= 0 {
				return nil, fmt.Errorf("modelregistry: alias %q of %q has non-positive output_limit", a.Name, m.Canonical)
			}
			spec := base // herda tudo do pai...
			spec.Canonical = m.Canonical
			if a.ContextWindow != nil {
				spec.ContextWindow = *a.ContextWindow
			}
			if a.OutputLimit != nil {
				spec.OutputLimit = *a.OutputLimit
			}
			if len(a.Modalities) > 0 {
				spec.Modalities = a.Modalities
			}
			if err := r.add(a.Name, spec); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

// Lookup retorna a spec para o nome exato (canônico ou alias). O slice
// Modalities do retorno é uma CÓPIA defensiva — mutar o retorno não altera
// o dicionário.
func (r *Registry) Lookup(name string) (Spec, bool) {
	spec, ok := r.byName[name]
	if !ok {
		return Spec{}, false
	}
	spec.Modalities = append([]string(nil), spec.Modalities...)
	return spec, true
}

// add indexa a spec sob o nome de consulta; duplicata é erro de conteúdo.
func (r *Registry) add(name string, spec Spec) error {
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("modelregistry: duplicate name %q in dictionary", name)
	}
	spec.Modalities = append([]string(nil), spec.Modalities...)
	r.byName[name] = spec
	return nil
}
