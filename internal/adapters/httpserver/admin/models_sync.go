package admin

import (
	"context"
	"net/http"

	"github.com/hacklabr/ai-pool/internal/modelsync"
)

// =============================================================================
// Sync do catálogo de modelos sob demanda (issue #25).
//
// O botão "Sincronizar agora" da página admin de modelos dispara uma rodada
// completa do modelsync (RF-11, ADR-0015) sem esperar o próximo tick do job
// periódico. A lógica de sincronização vive em internal/modelsync; aqui fica
// apenas a porta consumer-side, o handler HTTP e o DTO de resposta.
//
// Semântica fail-safe preservada: provedores individuais que falham viram
// skipped no relatório (200); apenas falha estrutural (DB) retorna erro
// (5XX no envelope padrão da API admin).
// =============================================================================

// ModelSyncRunner é a porta consumer-side para disparar uma rodada de sync
// do catálogo de modelos. Satisfeita pelo *modelsync.Syncer (mesma instância
// do job periódico e do CLI --sync-models no wiring do main).
type ModelSyncRunner interface {
	SyncAll(ctx context.Context) (*modelsync.Report, error)
}

// syncProviderResult é o resultado por provedor na resposta do sync.
// Contagens (não listas) — suficiente para o resumo da UI e mais leve.
type syncProviderResult struct {
	Slug        string `json:"slug"`
	Added       int    `json:"added"`
	Reactivated int    `json:"reactivated"`
	Deactivated int    `json:"deactivated"`
	Enriched    int    `json:"enriched"`
	Skipped     bool   `json:"skipped"`
	SkipReason  string `json:"skip_reason,omitempty"`
}

// syncResponse é o envelope de POST /api/web/admin/models/sync.
type syncResponse struct {
	Providers []syncProviderResult `json:"providers"`
	Summary   syncSummary          `json:"summary"`
}

// syncSummary totais agregados da rodada (espelha Report.Totals).
type syncSummary struct {
	Providers   int `json:"providers"`
	Added       int `json:"added"`
	Reactivated int `json:"reactivated"`
	Deactivated int `json:"deactivated"`
	Enriched    int `json:"enriched"`
	Skipped     int `json:"skipped"`
}

// buildSyncResponse converte o Report do modelsync no DTO de resposta da API.
func buildSyncResponse(report *modelsync.Report) syncResponse {
	resp := syncResponse{Providers: make([]syncProviderResult, 0, len(report.Providers))}
	for _, p := range report.Providers {
		resp.Providers = append(resp.Providers, syncProviderResult{
			Slug:        p.ProviderSlug,
			Added:       len(p.Added),
			Reactivated: len(p.Reactivated),
			Deactivated: len(p.Deactivated),
			Enriched:    len(p.Enriched),
			Skipped:     p.Skipped,
			SkipReason:  p.SkipReason,
		})
	}
	added, reactivated, deactivated, enriched, skipped := report.Totals()
	resp.Summary = syncSummary{
		Providers:   len(report.Providers),
		Added:       added,
		Reactivated: reactivated,
		Deactivated: deactivated,
		Enriched:    enriched,
		Skipped:     skipped,
	}
	return resp
}

// handleAdminSyncModels executa uma rodada completa de sincronização do
// catálogo de modelos sob demanda (botão "Sincronizar agora").
//
// Rota: POST /api/web/admin/models/sync.
// Autenticação: withAdminAuth (session + CSRF + role=admin).
//
// O syncer aplica timeout de 30s por provedor internamente; usamos o request
// context como está (sem timeout adicional no handler). A rodada pode levar
// dezenas de segundos com muitos provedores — a UI desabilita o botão durante
// a execução (hx-disabled-elt).
func (a *App) handleAdminSyncModels(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.modelSyncRunner == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_configured",
			"model sync not configured")
		return
	}
	report, err := a.modelSyncRunner.SyncAll(r.Context())
	if err != nil {
		// Falha estrutural (DB durante a rodada) — envelope padrão da API.
		writeAPIError(w, http.StatusInternalServerError, "internal", "model sync failed")
		return
	}
	writeJSON(w, http.StatusOK, buildSyncResponse(report))
}
