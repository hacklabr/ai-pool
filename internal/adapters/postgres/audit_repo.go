package postgres

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditEntry é um evento de auditoria (spec §3.4.1 item 10, Security §10.5).
//
// Append-ONLY: o schema e os GRANTs garantem que UPDATE/DELETE nunca ocorram
// pela role da aplicação (aipp_app tem apenas INSERT em audit_log). A tabela
// NÃO tem FK física — pode referenciar entidades deletadas no futuro, e isto é
// desejado (o log registra o que aconteceu naquele momento histórico).
type AuditEntry struct {
	OrganizationID int64
	ActorUserID    *int64 // nil para ações do sistema (ex.: scheduler)
	Action         string // ex.: "provider_key.disable", "user.update"
	EntityType     string // ex.: "user", "provider_key", "api_token"
	EntityID       *int64
	Metadata       any // JSONB: contexto livre (antes/depois, motivo...)
	IP             net.IP
	UserAgent      string
}

// AuditRepo persiste eventos de auditoria (append-only).
//
// Apenas INSERT é exposto — nunca UPDATE/DELETE. Isto é reforçado em duas
// camadas: (1) o repositório não tem métodos de mutação além de Append;
// (2) o GRANT no DB (responsabilidade do Helm/IaC) revoga UPDATE/DELETE.
type AuditRepo struct {
	pool *pgxpool.Pool
}

// NewAuditRepo cria o repositório.
func NewAuditRepo(pool *pgxpool.Pool) *AuditRepo { return &AuditRepo{pool: pool} }

// Append insere um evento de auditoria. Idempotente apenas no sentido de que
// duplicatas (mesma action/entity/timestamp) são aceitas — o caller é responsável
// por não chamar Append duas vezes para o mesmo evento lógico.
//
// metadata pode ser nil, uma struct, um map[string]any ou um []byte JSON.
// O pgx serializa via driver para JSONB; para structs, use json.Marshal antes
// se quiser controle total do formato.
func (r *AuditRepo) Append(ctx context.Context, e AuditEntry) error {
	const q = `
		INSERT INTO audit_log (organization_id, actor_user_id, action, entity_type,
		                       entity_id, metadata, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := r.pool.Exec(ctx, q,
		e.OrganizationID,
		optInt64(e.ActorUserID),
		e.Action,
		e.EntityType,
		optInt64(e.EntityID),
		e.Metadata,
		optIP(e.IP),
		optStr(e.UserAgent),
	)
	if err != nil {
		return fmt.Errorf("AuditRepo.Append: %w", err)
	}
	return nil
}

// optInt64 converte *int64 para pgtype.Int8 (Valid=false se nil).
func optInt64(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{Valid: false}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// optStr converte string vazia para NULL.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optIP converte net.IP nil para NULL.
func optIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	return ip
}

// =============================================================================
// Leitura do audit_log para a tela de Auditoria (F8).
//
// Append-only é garantido pela AUSÊNCIA de métodos Update/Delete neste repo E
// pelos GRANTs do DB (role aipp_app só tem INSERT em audit_log). A leitura
// (List abaixo) usa SELECT — permitido para aipp_app (GRANT SELECT em all tables).
// =============================================================================

// AuditLogEntry é uma linha de audit_log lida para exibição na UI.
//
// Metadata é exposta como []byte (JSON cru) — a UI decide como formatar
// (ex.: syntax highlight em modal). ActorName é resolvido via LEFT JOIN
// com users (fallback "(system)" para ações sem ator — ex.: scheduler).
type AuditLogEntry struct {
	ID          int64
	ActorUserID *int64
	ActorName   string // username do ator, ou "(system)" se actor_user_id IS NULL
	Action      string // ex.: "provider_key.disable"
	EntityType  string // ex.: "user", "provider_key"
	EntityID    *int64
	Metadata    []byte // JSONB cru (pode ser nil)
	IP          string // string vazia se NULL
	UserAgent   string // string vazia se NULL
	CreatedAt   time.Time
}

// AuditFilter expressa os filtros opcionais para List.
// Campos zero-value significam "sem filtro nesta dimensão".
type AuditFilter struct {
	ActorUserID *int64     // filtra por ator (ex.: histórico de um usuário)
	Action      string     // filtra por ação exata (ex.: "user.update")
	EntityType  string     // filtra por tipo de entidade
	From        *time.Time // período inicial (inclusivo)
	To          *time.Time // período final (exclusivo)
}

// List retorna uma página de audit_log (ordenada por created_at DESC) e o total
// de entradas que casam com o filtro (para paginação na UI).
//
// limit é clampado em [1, 200]; offset em [0, total]. Retorna slice vazio (não
// nil) se não houver entradas — caller pode iterar sem nil-check.
//
// Implementação: monta a query dinamicamente com placeholders $N conforme os
// filtros presentes. Para o MVP (filtro admin via HTMX), a complexidade é
// aceitável — não há necessidade de query builder.
func (r *AuditRepo) List(ctx context.Context, orgID int64, filter AuditFilter, limit, offset int) ([]AuditLogEntry, int, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	// Monta WHERE clause dinâmico.
	where := []string{"a.organization_id = $1"}
	args := []any{orgID}
	argN := 2 // próximo placeholder
	addFilter := func(clause string, val any) {
		where = append(where, fmt.Sprintf(clause, argN))
		args = append(args, val)
		argN++
	}
	if filter.ActorUserID != nil {
		addFilter("a.actor_user_id = $%d", *filter.ActorUserID)
	}
	if filter.Action != "" {
		addFilter("a.action = $%d", filter.Action)
	}
	if filter.EntityType != "" {
		addFilter("a.entity_type = $%d", filter.EntityType)
	}
	if filter.From != nil {
		addFilter("a.created_at >= $%d", *filter.From)
	}
	if filter.To != nil {
		addFilter("a.created_at < $%d", *filter.To)
	}
	whereClause := joinStrings(where, " AND ")

	// Query principal: LEFT JOIN com users para resolver ActorName.
	listQuery := fmt.Sprintf(`
		SELECT a.id, a.actor_user_id, COALESCE(u.username, '(system)'),
		       a.action, a.entity_type, a.entity_id,
		       COALESCE(a.metadata::text, ''), COALESCE(host(a.ip), ''),
		       COALESCE(a.user_agent, ''), a.created_at
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.actor_user_id
		WHERE %s
		ORDER BY a.created_at DESC
		LIMIT $%d OFFSET $%d`, whereClause, argN, argN+1)
	listArgs := append(args, limit, offset)

	rows, err := r.pool.Query(ctx, listQuery, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("AuditRepo.List query: %w", err)
	}
	defer rows.Close()

	out := make([]AuditLogEntry, 0, limit)
	for rows.Next() {
		var e AuditLogEntry
		var metadata string
		var ipStr string
		if err := rows.Scan(
			&e.ID, &e.ActorUserID, &e.ActorName,
			&e.Action, &e.EntityType, &e.EntityID,
			&metadata, &ipStr, &e.UserAgent, &e.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("AuditRepo.List scan: %w", err)
		}
		if metadata != "" {
			e.Metadata = []byte(metadata)
		}
		e.IP = ipStr
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("AuditRepo.List rows.Err: %w", err)
	}

	// Count total (para paginação). Mesma WHERE, sem LIMIT/OFFSET.
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM audit_log a WHERE %s`, whereClause)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("AuditRepo.List count: %w", err)
	}

	return out, total, nil
}

// joinStrings concatena strings com sep. Implementação local leve (evita
// importar strings só para isto neste arquivo — strings já é usado em helpers.go).
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// =============================================================================
// Stubs para fases futuras:
//
//   - QueryByEntity(orgID, entityType, entityID)     -> histórico de entidade.
//   - ExportForCompliance(orgID, from, to)           -> LGPD export (roadmap).
//
// IMPORTANTE: NUNCA implementar Update/Delete — quebra a propriedade append-only
// e a integridade de auditoria (Repudiation defense, spec §3.3.7).
// =============================================================================
