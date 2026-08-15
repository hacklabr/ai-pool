// Package modelsync sincroniza o catálogo local de modelos (tabela models)
// com a listagem exposta por cada provedor (GET {base_url}/models, formato
// OpenAI). RF-11, ADR-0015.
//
// Princípios (ADR-0015):
//   - FAIL-SAFE: provedor indisponível/sem chave usável/resposta inválida →
//     NENHUMA alteração nos modelos daquele provedor (log sanitizado).
//   - Reconciliação não-destrutiva: adiciona novos (ativos), reativa os que
//     retornaram, DESATIVA os ausentes. NUNCA deleta linhas; NUNCA altera
//     context_window/output_limit/modalities/pricing/aliases PREENCHIDOS de
//     existentes (enriquecimento manual é protegido). Exceção controlada
//     (issue #26): com um SpecSource injetado, campos VAZIOS de existentes
//     recebem backfill do dicionário interno — só nulo/vazio, nunca
//     sobrescreve (BackfillSpecs garante a invariante no SQL).
//   - O pacote vive FORA do domain (que continua PURO): declara portas
//     mínimas consumer-side, satisfeitas pelos adapters postgres/crypto.
//
// Segurança: chaves decifradas vivem apenas em memória durante o request ao
// provedor e são zeroizadas em seguida; nunca são logadas (o logger injetado
// passa pelo SanitizingHandler do main; este pacote nunca usa slog.Default()).
package modelsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/modelregistry"
)

// defaultProviderTimeout é o tempo máximo da chamada GET /models por provedor.
const defaultProviderTimeout = 30 * time.Second

// maxModelsResponseBytes limita o body de listagem de modelos (defesa DoS).
const maxModelsResponseBytes = 4 * 1024 * 1024

// =============================================================================
// Portas mínimas (consumer-side). Os adapters postgres/crypto existentes
// satisfazem estas interfaces sem alteração.
// =============================================================================

// ProviderLister lista os provedores ativos da organização.
// Satisfeita por postgres.ProviderRepo.ListActive.
type ProviderLister interface {
	ListActive(ctx context.Context, orgID domain.OrganizationID) ([]domain.Provider, error)
}

// KeyLister lista as chaves usáveis (status enabled) de um provedor.
// Satisfeita por postgres.KeyRepo.ListUsableByProvider.
type KeyLister interface {
	ListUsableByProvider(ctx context.Context, orgID domain.OrganizationID, providerID domain.ID) ([]domain.ProviderKey, error)
}

// ModelStore lê e reconcilia o catálogo local de modelos.
// Satisfeita por postgres.ModelRepo (ListAll/Create/SetActive/BackfillSpecs).
type ModelStore interface {
	ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.Model, error)
	Create(ctx context.Context, m *domain.Model) (domain.ID, error)
	SetActive(ctx context.Context, orgID domain.OrganizationID, id domain.ID, active bool) error
	// BackfillSpecs preenche APENAS campos vazios (issue #26): nunca
	// sobrescreve valores existentes (invariante do adapter postgres).
	BackfillSpecs(ctx context.Context, orgID domain.OrganizationID, id domain.ID, contextWindow, outputLimit *int, modalities []string) (bool, error)
}

// SpecSource é a porta consumer-side do dicionário interno de especificações
// (issue #26). Satisfeita por *modelregistry.Registry; nil = comportamento
// anterior ao dicionário (modelos nascem só com o nome, sem backfill).
type SpecSource interface {
	Lookup(name string) (modelregistry.Spec, bool)
}

// =============================================================================
// Relatório do sync (usado pelo CLI --sync-models e pelos logs).
// =============================================================================

// ProviderReport agrega o resultado do sync de UM provedor.
type ProviderReport struct {
	ProviderSlug string
	Added        []string // modelos criados (ativos)
	Reactivated  []string // modelos que voltaram ao upstream e foram reativados
	Deactivated  []string // modelos locais ausentes no upstream, desativados
	// Enriched: modelos existentes que ganharam backfill do dicionário
	// interno de specs (issue #26) — campos vazios preenchidos.
	Enriched []string
	// Skipped=true quando o provedor não pôde ser consultado (fail-safe):
	// nenhuma alteração foi feita nos modelos dele; SkipReason traz o motivo.
	Skipped    bool
	SkipReason string
}

// Report agrega o resultado de uma rodada de sincronização.
type Report struct {
	Providers []ProviderReport
}

// Totals soma added/reactivated/deactivated/enriched/skipped de todos os provedores.
func (r *Report) Totals() (added, reactivated, deactivated, enriched, skipped int) {
	for _, p := range r.Providers {
		added += len(p.Added)
		reactivated += len(p.Reactivated)
		deactivated += len(p.Deactivated)
		enriched += len(p.Enriched)
		if p.Skipped {
			skipped++
		}
	}
	return added, reactivated, deactivated, enriched, skipped
}

// String formata o relatório para saída humana (CLI).
func (r *Report) String() string {
	var b strings.Builder
	if len(r.Providers) == 0 {
		b.WriteString("no active providers; nothing to do\n")
		return b.String()
	}
	for _, p := range r.Providers {
		if p.Skipped {
			fmt.Fprintf(&b, "provider %s: skipped (%s)\n", p.ProviderSlug, p.SkipReason)
			continue
		}
		fmt.Fprintf(&b, "provider %s: added=%d reactivated=%d deactivated=%d enriched=%d\n",
			p.ProviderSlug, len(p.Added), len(p.Reactivated), len(p.Deactivated), len(p.Enriched))
	}
	return b.String()
}

// =============================================================================
// Syncer
// =============================================================================

// Syncer executa a sincronização do catálogo.
type Syncer struct {
	providers ProviderLister
	keys      KeyLister
	models    ModelStore
	decryptor domain.KeyDecryptor
	client    *http.Client
	logger    *slog.Logger
	orgID     domain.OrganizationID
	// registry é o dicionário interno de specs (issue #26); nil = modelos
	// nascem só com o nome e existentes não recebem backfill (comportamento
	// anterior). Injetado via SetSpecSource (o mesmo *Registry serve job,
	// CLI --sync-models e botão admin).
	registry SpecSource
	// providerTimeout limita o GET /models de cada provedor (default 30s).
	providerTimeout time.Duration
}

// NewSyncer cria o sincronizador. client deve ser o http.Client compartilhado
// do upstream (proxy.NewUpstreamClient); decryptor é o AES-GCM (prod) ou
// Identity (dev).
func NewSyncer(
	providers ProviderLister,
	keys KeyLister,
	models ModelStore,
	decryptor domain.KeyDecryptor,
	client *http.Client,
	logger *slog.Logger,
	orgID domain.OrganizationID,
) *Syncer {
	return &Syncer{
		providers:       providers,
		keys:            keys,
		models:          models,
		decryptor:       decryptor,
		client:          client,
		logger:          logger,
		orgID:           orgID,
		providerTimeout: defaultProviderTimeout,
	}
}

// SetSpecSource injeta o dicionário interno de especificações (issue #26).
// Opcional: sem chamada (ou fonte nil), o sync comporta-se como antes do
// dicionário. Deve ser chamado antes do primeiro SyncAll (wiring no main).
func (s *Syncer) SetSpecSource(src SpecSource) *Syncer {
	s.registry = src
	return s
}

// SyncAll executa uma rodada completa de sincronização.
//
// Retorna erro APENAS em falha estrutural (DB indisponível ao listar
// provedores/modelos). Falhas individuais de provedor são fail-safe: constam
// no relatório como skipped e NÃO retornam erro.
func (s *Syncer) SyncAll(ctx context.Context) (*Report, error) {
	providers, err := s.providers.ListActive(ctx, s.orgID)
	if err != nil {
		return nil, fmt.Errorf("modelsync: list active providers: %w", err)
	}
	// Catálogo local lido UMA vez (organização; volume pequeno) e agrupado por
	// provedor. Falha aqui é estrutural (DB) — aborta a rodada.
	allModels, err := s.models.ListAll(ctx, s.orgID)
	if err != nil {
		return nil, fmt.Errorf("modelsync: list models: %w", err)
	}
	localByProvider := make(map[domain.ID][]domain.Model)
	for _, m := range allModels {
		localByProvider[m.ProviderID] = append(localByProvider[m.ProviderID], m)
	}

	report := &Report{}
	for _, p := range providers {
		report.Providers = append(report.Providers, s.syncProvider(ctx, p, localByProvider[p.ID]))
	}

	added, reactivated, deactivated, enriched, skipped := report.Totals()
	s.logger.Info("model sync completed",
		"providers", len(providers),
		"added", added,
		"reactivated", reactivated,
		"deactivated", deactivated,
		"enriched", enriched,
		"skipped", skipped,
	)
	return report, nil
}

// syncProvider sincroniza um provedor. FAIL-SAFE: qualquer falha de rede,
// timeout, HTTP != 200, ausência de chave usável ou erro de decriptagem
// resulta em skipped (zero alterações).
//
// Rotação de chaves (issue #30): HTTP 401/403 é falha DAQUELA chave (sem
// crédito, revogada) — tentamos a PRÓXIMA chave usável até esgotar todas;
// só então o provedor é skipado. Falhas não-autorizacionais (rede, timeout,
// 5xx do provedor) não dependem da chave: abortam sem girar o pool.
func (s *Syncer) syncProvider(ctx context.Context, p domain.Provider, local []domain.Model) ProviderReport {
	rep := ProviderReport{ProviderSlug: p.Slug}
	skip := func(reason string) ProviderReport {
		rep.Skipped = true
		rep.SkipReason = reason
		s.logger.Warn("model sync: provider skipped (no changes applied)",
			"provider", p.Slug, "reason", reason)
		return rep
	}

	// 1. Chaves usáveis do provedor (rotacionamos em 401/403).
	keys, err := s.keys.ListUsableByProvider(ctx, s.orgID, p.ID)
	if err != nil {
		return skip("failed to list keys")
	}
	if len(keys) == 0 {
		return skip("no usable key")
	}

	// 2..n. Para cada chave: decripta (em memória; zeroizada ao final da
	// tentativa), consulta o upstream e decide entre reconciliar, girar para
	// a próxima chave ou abortar o provedor.
	var lastAuthFail string
	for _, key := range keys {
		plain, err := s.decryptor.Decrypt(ctx, key)
		if err != nil {
			// Falha de decrypt é daquela chave (ex.: KEK antiga) — gira.
			s.logger.Warn("model sync: decrypt failed for key, trying next",
				"provider", p.Slug, "err", err)
			continue
		}
		ids, status, ferr := s.fetchModelIDs(ctx, p.BaseURL, string(plain))
		for i := range plain {
			plain[i] = 0
		}
		if ferr == nil {
			// Provedor OK com esta chave → reconcilia.
			return s.reconcile(ctx, p, ids, local, rep)
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			lastAuthFail = fmt.Sprintf("key auth failed (HTTP %d)", status)
			s.logger.Warn("model sync: key rejected by upstream, trying next key",
				"provider", p.Slug, "status", status)
			continue
		}
		// Falha não-autorizacional (rede/timeout/5xx/parse): independe da
		// chave — aborta sem consumir o resto do pool de chaves.
		return skip(ferr.Error())
	}
	if lastAuthFail != "" {
		return skip(fmt.Sprintf("all %d usable keys rejected (%s)", len(keys), lastAuthFail))
	}
	return skip("no usable key could be decrypted")
}

// fetchModelIDs chama GET {baseURL}/models e faz parse tolerante do formato
// OpenAI {"data":[{"id":"..."}]}: entradas sem id são ignoradas; campos extras
// são ignorados. O status HTTP acompanha o retorno para o caller decidir a
// rotação de chaves (401/403 = falha da chave; 0 = falha antes de resposta).
// Qualquer erro de rede/timeout/HTTP != 200/JSON inválido é retornado como
// erro (o caller aplica o fail-safe).
func (s *Syncer) fetchModelIDs(ctx context.Context, baseURL, bearerToken string) ([]string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid base_url")
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("upstream request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drena um pedaço do body para permitir reuse da conexão.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read upstream response")
	}

	// Parse tolerante: apenas data[].id importa.
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("invalid response payload")
	}

	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids, http.StatusOK, nil
}

// reconcile aplica a diferença upstream × catálogo local (provedor OK):
//   - id existe e inativo     → SetActive(true)  (reactivated)
//   - id não existe           → Create (Name=id, DisplayName=id, IsActive=true;
//     com registry configurado e spec disponível, nasce JÁ com
//     context_window/output_limit/modalities do dicionário — issue #26) (added)
//   - id existe               → backfill: campos VAZIOS preenchidos com a spec
//     do dicionário, se disponível (enriched). Valores existentes NUNCA são
//     tocados (BackfillSpecs garante no SQL).
//   - modelo local ausente no upstream e ativo → SetActive(false) (deactivated)
//
// NUNCA deleta; NUNCA sobrescreve ContextWindow/OutputLimit/Modalities/
// pricing/aliases de modelos existentes (enriquecimento manual preservado,
// ADR-0015 — o dicionário só preenche campo vazio).
func (s *Syncer) reconcile(ctx context.Context, p domain.Provider, upstreamIDs []string, local []domain.Model, rep ProviderReport) ProviderReport {
	localByName := make(map[string]domain.Model, len(local))
	for _, m := range local {
		localByName[m.Name] = m
	}

	inUpstream := make(map[string]bool, len(upstreamIDs))
	for _, id := range upstreamIDs {
		if inUpstream[id] {
			continue // dedup de ids repetidos no upstream
		}
		inUpstream[id] = true

		lm, exists := localByName[id]
		switch {
		case !exists:
			displayName := id
			newModel := domain.Model{
				ProviderID:  p.ID,
				OrgID:       s.orgID,
				Name:        id,
				DisplayName: &displayName,
				IsActive:    true,
			}
			// Issue #26: modelo presente no dicionário nasce configurado.
			if s.registry != nil {
				if spec, ok := s.registry.Lookup(id); ok {
					cw, ol := spec.ContextWindow, spec.OutputLimit
					newModel.ContextWindow = &cw
					newModel.OutputLimit = &ol
					newModel.Modalities = spec.Modalities
				}
			}
			_, err := s.models.Create(ctx, &newModel)
			if err != nil {
				// Ex.: unique violation em race com outro sync — benigno; o
				// próximo tick reconcilia. Não interrompe a rodada.
				s.logger.Warn("model sync: create model failed",
					"provider", p.Slug, "model", id, "err", err)
				continue
			}
			rep.Added = append(rep.Added, id)
		case !lm.IsActive:
			if err := s.models.SetActive(ctx, s.orgID, lm.ID, true); err != nil {
				s.logger.Warn("model sync: reactivate model failed",
					"provider", p.Slug, "model", id, "err", err)
				continue
			}
			rep.Reactivated = append(rep.Reactivated, id)
			s.backfillSpecs(ctx, p, lm, &rep)
		default:
			// exists && IsActive → inalterado pelo reconcile base; tenta
			// apenas o backfill do dicionário (no-op se nada a preencher).
			s.backfillSpecs(ctx, p, lm, &rep)
		}
	}

	for _, m := range local {
		if inUpstream[m.Name] || !m.IsActive {
			continue
		}
		if err := s.models.SetActive(ctx, s.orgID, m.ID, false); err != nil {
			s.logger.Warn("model sync: deactivate model failed",
				"provider", p.Slug, "model", m.Name, "err", err)
			continue
		}
		rep.Deactivated = append(rep.Deactivated, m.Name)
	}

	s.logger.Info("model sync: provider reconciled",
		"provider", p.Slug,
		"added", len(rep.Added),
		"reactivated", len(rep.Reactivated),
		"deactivated", len(rep.Deactivated),
		"enriched", len(rep.Enriched),
	)
	return rep
}

// backfillSpecs preenche campos VAZIOS de um modelo existente com a spec do
// dicionário (issue #26). Regra invariante (ADR-0015): valor existente
// (manual ou de sync anterior) NUNCA é sobrescrito — só campos nulos/vazios
// são oferecidos ao repo, e o adapter garante a semântica no SQL.
func (s *Syncer) backfillSpecs(ctx context.Context, p domain.Provider, m domain.Model, rep *ProviderReport) {
	if s.registry == nil {
		return
	}
	spec, ok := s.registry.Lookup(m.Name)
	if !ok {
		return
	}
	// Oferece apenas o que está vazio no modelo local.
	var cw, ol *int
	if m.ContextWindow == nil {
		c := spec.ContextWindow
		cw = &c
	}
	if m.OutputLimit == nil {
		o := spec.OutputLimit
		ol = &o
	}
	var modalities []string
	if len(m.Modalities) == 0 {
		modalities = spec.Modalities
	}
	if cw == nil && ol == nil && modalities == nil {
		return // nada a preencher (modelo já configurado)
	}
	enriched, err := s.models.BackfillSpecs(ctx, s.orgID, m.ID, cw, ol, modalities)
	if err != nil {
		// Falha pontual de DB: loga e segue — o próximo tick tenta de novo.
		s.logger.Warn("model sync: backfill specs failed",
			"provider", p.Slug, "model", m.Name, "err", err)
		return
	}
	if enriched {
		rep.Enriched = append(rep.Enriched, m.Name)
	}
}
