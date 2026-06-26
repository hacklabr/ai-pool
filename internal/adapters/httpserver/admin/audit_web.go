package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/rafael/ai-pool/internal/adapters/postgres"
)

// =============================================================================
// Endpoint JSON para a tela de Auditoria (F8).
//
// GET /api/web/admin/audit?limit=&offset=&action=&entity_type=
//
// Retorna entradas paginadas do audit_log (append-only). Consumido pelo
// template pages/audit.html via HTMX/Alpine para paginação e filtros sem
// recarregar a página inteira.
//
// Segurança:
//   - Acesso exclusivo role=admin (withAdminAuth).
//   - Sem exposição de dados sensíveis: o audit_log NÃO contém secrets (apenas
//     metadados como action, entity_id, IP — ver Security §10.1).
//   - IP é retornado como string simples (já sanitizado pelo repositório).
// =============================================================================

// postgresAuditFilterView é um alias para postgres.AuditFilter, exposto para
// pages.go referenciar sem importar o pacote postgres diretamente. Mantém o
// acoplamento baixo entre handlers e a camada de persistência.
type postgresAuditFilterView = postgres.AuditFilter

// auditEntryUI é a representação UI de uma entrada de audit_log.
//
// Diferente do tipo interno postgres.AuditLogEntry (que usa []byte para
// metadata), este usa string para facilitar marshal JSON e interpolação no
// template HTML (html/template escapa automaticamente).
type auditEntryUI struct {
	ID          int64     `json:"id"`
	ActorUserID *int64    `json:"actor_user_id,omitempty"`
	ActorName   string    `json:"actor_name"`
	Action      string    `json:"action"`
	EntityType  string    `json:"entity_type"`
	EntityID    *int64    `json:"entity_id,omitempty"`
	Metadata    string    `json:"metadata"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
	CreatedAt   time.Time `json:"created_at"`
}

// auditDataResponse é o payload paginado do GET /api/web/admin/audit.
type auditDataResponse struct {
	Entries []auditEntryUI `json:"entries"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
}

// handleAdminAuditData retorna uma página do audit_log em JSON.
//
// Query params:
//   - limit (default 50, max 200)
//   - offset (default 0)
//   - action (filtro por ação exata, ex.: "user.update")
//   - entity_type (filtro por tipo, ex.: "provider_key")
//
// Ordenação fixa: created_at DESC (mais recente primeiro). Não há filtros por
// ator no MVP (actor_user_id exige lookup por username primeiro — roadmap).
func (a *App) handleAdminAuditData(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.auditRepo == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "audit repo not initialized")
		return
	}

	limit, offset := parseLimitOffset(r)
	f := postgres.AuditFilter{
		Action:     strings.TrimSpace(r.URL.Query().Get("action")),
		EntityType: strings.TrimSpace(r.URL.Query().Get("entity_type")),
	}

	entries, total, err := a.auditRepo.List(r.Context(), sess.OrgID, f, limit, offset)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not load audit log")
		return
	}

	out := make([]auditEntryUI, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditEntryUI{
			ID:          e.ID,
			ActorUserID: e.ActorUserID,
			ActorName:   e.ActorName,
			Action:      e.Action,
			EntityType:  e.EntityType,
			EntityID:    e.EntityID,
			Metadata:    string(e.Metadata),
			IP:          e.IP,
			UserAgent:   e.UserAgent,
			CreatedAt:   e.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, auditDataResponse{
		Entries: out,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
	})
}
