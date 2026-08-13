package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/modelcatalog"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// GET /v1/models (e /api/models) — listagem agregada do catálogo LOCAL (RF-12).
//
// Responde a partir do banco (modelos ativos de providers ativos da org do
// token autenticado) — NÃO consulta os provedores em tempo real. O catálogo
// local é mantido atualizado pelo modelsync (RF-11, ADR-0015).
//
// Formatos:
//   - default: OpenAI {"object":"list","data":[{id,object,created,owned_by}]}
//   - ?format=opencode: mapa no schema OpenCode (provider.models.{id}), mesmo
//     formato retornado no aceite de convite (modelcatalog.OpenCodeModelsMap).
//
// Dedup por nome: se dois providers ativos oferecem o mesmo model, o PRIMEIRO
// (menor provider id) vence — espelha GetActiveProviderForModel (ORDER BY p.id).
//
// Auth: o MESMO AuthMiddleware do proxy (Bearer sk-aipp-...) — 401 sem token.
// =============================================================================

// modelCatalogLister é a porta mínima (consumer-side) de leitura do catálogo.
// Satisfeita por postgres.ModelRepo.ListAll.
type modelCatalogLister interface {
	ListAll(ctx context.Context, orgID domain.OrganizationID) ([]domain.Model, error)
}

// activeProviderLister é a porta mínima de listagem de providers ativos.
// Satisfeita por postgres.ProviderRepo.ListActive.
type activeProviderLister interface {
	ListActive(ctx context.Context, orgID domain.OrganizationID) ([]domain.Provider, error)
}

// SetModelCatalog injeta as fontes do catálogo local para GET /v1/models.
// Se não chamado (ex.: dev sem DB), o endpoint responde 503.
func (s *Server) SetModelCatalog(models modelCatalogLister, providers activeProviderLister) {
	s.modelLister = models
	s.providerLister = providers
}

// handleListModels responde GET /v1/models e GET /api/models.
// Rota protegida pelo AuthMiddleware — userFromContext é garantido.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if s.modelLister == nil || s.providerLister == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, ErrTypeServer,
			"model catalog is not available", "service_unavailable")
		return
	}
	user := userFromContext(r.Context())

	providers, err := s.providerLister.ListActive(r.Context(), user.OrgID)
	if err != nil {
		s.logger.Warn("list models: list active providers failed", "err", err)
		writeOpenAIError(w, http.StatusInternalServerError, ErrTypeServer,
			"failed to load model catalog", "internal_error")
		return
	}
	models, err := s.modelLister.ListAll(r.Context(), user.OrgID)
	if err != nil {
		s.logger.Warn("list models: list models failed", "err", err)
		writeOpenAIError(w, http.StatusInternalServerError, ErrTypeServer,
			"failed to load model catalog", "internal_error")
		return
	}

	// Indexa modelos ativos por provider.
	modelsByProvider := make(map[domain.ID][]domain.Model)
	for _, m := range models {
		if !m.IsActive {
			continue
		}
		modelsByProvider[m.ProviderID] = append(modelsByProvider[m.ProviderID], m)
	}

	// Providers ordenados por id: o PRIMEIRO provider ativo vence o dedup por
	// nome (mesma precedência do hot path, ORDER BY p.id).
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })

	seen := make(map[string]bool)
	var deduped []domain.Model
	var entries []modelcatalog.Entry
	for _, p := range providers {
		for _, m := range modelsByProvider[p.ID] {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			deduped = append(deduped, m)
			entries = append(entries, modelcatalog.Entry{
				Name:         m.Name,
				ProviderSlug: p.Slug,
				CreatedUnix:  m.CreatedAt.Unix(),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.URL.Query().Get("format") == "opencode" {
		_ = json.NewEncoder(w).Encode(modelcatalog.OpenCodeModelsMap(deduped))
		return
	}
	_ = json.NewEncoder(w).Encode(modelcatalog.OpenAIList(entries))
}
