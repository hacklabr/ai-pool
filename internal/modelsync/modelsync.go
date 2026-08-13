// Package modelsync sincroniza o catálogo local de modelos (tabela models)
// com a listagem exposta por cada provedor (GET {base_url}/models, formato
// OpenAI). RF-11, ADR-0015.
//
// Princípios (ADR-0015):
//   - FAIL-SAFE: provedor indisponível/sem chave usável/resposta inválida →
//     NENHUMA alteração nos modelos daquele provedor (log sanitizado).
//   - Reconciliação não-destrutiva: adiciona novos (ativos), reativa os que
//     retornaram, DESATIVA os ausentes. NUNCA deleta linhas; NUNCA altera
//     context_window/output_limit/modalities/pricing/aliases de existentes
//     (enriquecimento manual é protegido).
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
// Satisfeita por postgres.ModelRepo (ListAll/Create/SetActive).
type ModelStore interface {
	ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.Model, error)
	Create(ctx context.Context, m *domain.Model) (domain.ID, error)
	SetActive(ctx context.Context, orgID domain.OrganizationID, id domain.ID, active bool) error
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
	// Skipped=true quando o provedor não pôde ser consultado (fail-safe):
	// nenhuma alteração foi feita nos modelos dele; SkipReason traz o motivo.
	Skipped    bool
	SkipReason string
}

// Report agrega o resultado de uma rodada de sincronização.
type Report struct {
	Providers []ProviderReport
}

// Totals soma added/reactivated/deactivated/skipped de todos os provedores.
func (r *Report) Totals() (added, reactivated, deactivated, skipped int) {
	for _, p := range r.Providers {
		added += len(p.Added)
		reactivated += len(p.Reactivated)
		deactivated += len(p.Deactivated)
		if p.Skipped {
			skipped++
		}
	}
	return added, reactivated, deactivated, skipped
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
		fmt.Fprintf(&b, "provider %s: added=%d reactivated=%d deactivated=%d\n",
			p.ProviderSlug, len(p.Added), len(p.Reactivated), len(p.Deactivated))
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

	added, reactivated, deactivated, skipped := report.Totals()
	s.logger.Info("model sync completed",
		"providers", len(providers),
		"added", added,
		"reactivated", reactivated,
		"deactivated", deactivated,
		"skipped", skipped,
	)
	return report, nil
}

// syncProvider sincroniza um provedor. FAIL-SAFE: qualquer falha de rede,
// timeout, HTTP != 200, JSON inválido, ausência de chave usável ou erro de
// decriptagem resulta em skipped (zero alterações).
func (s *Syncer) syncProvider(ctx context.Context, p domain.Provider, local []domain.Model) ProviderReport {
	rep := ProviderReport{ProviderSlug: p.Slug}
	skip := func(reason string) ProviderReport {
		rep.Skipped = true
		rep.SkipReason = reason
		s.logger.Warn("model sync: provider skipped (no changes applied)",
			"provider", p.Slug, "reason", reason)
		return rep
	}

	// 1. Uma chave usável do provedor.
	keys, err := s.keys.ListUsableByProvider(ctx, s.orgID, p.ID)
	if err != nil {
		return skip("failed to list keys")
	}
	if len(keys) == 0 {
		return skip("no usable key")
	}

	// 2. Decripta (em memória; zeroizada ao final).
	plain, err := s.decryptor.Decrypt(ctx, keys[0])
	if err != nil {
		return skip("failed to decrypt key")
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()

	// 3. GET {BaseURL}/models — o base_url já contém a versão (ex.: /v1).
	ids, err := s.fetchModelIDs(ctx, p.BaseURL, string(plain))
	if err != nil {
		return skip(err.Error())
	}

	// 4. Reconciliação (provedor OK).
	return s.reconcile(ctx, p, ids, local, rep)
}

// fetchModelIDs chama GET {baseURL}/models e faz parse tolerante do formato
// OpenAI {"data":[{"id":"..."}]}: entradas sem id são ignoradas; campos extras
// são ignorados. Qualquer erro de rede/timeout/HTTP != 200/JSON inválido é
// retornado como erro (o caller aplica o fail-safe).
func (s *Syncer) fetchModelIDs(ctx context.Context, baseURL, bearerToken string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url")
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drena um pedaço do body para permitir reuse da conexão.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read upstream response")
	}

	// Parse tolerante: apenas data[].id importa.
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid response payload")
	}

	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids, nil
}

// reconcile aplica a diferença upstream × catálogo local (provedor OK):
//   - id existe e inativo     → SetActive(true)  (reactivated)
//   - id não existe           → Create (Name=id, DisplayName=id, IsActive=true,
//     demais campos nulos/default) (added)
//   - modelo local ausente no upstream e ativo → SetActive(false) (deactivated)
//
// NUNCA deleta; NUNCA altera ContextWindow/OutputLimit/Modalities/pricing/
// aliases de modelos existentes (enriquecimento manual preservado, ADR-0015).
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
			_, err := s.models.Create(ctx, &domain.Model{
				ProviderID:  p.ID,
				OrgID:       s.orgID,
				Name:        id,
				DisplayName: &displayName,
				IsActive:    true,
			})
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
		}
		// exists && IsActive → inalterado (nada a fazer).
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
	)
	return rep
}
