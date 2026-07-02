package postgres

import (
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// =============================================================================
// Erros sentinelas do pacote postgres.
//
// Estes erros NÃO substituem os do domínio (domain.ErrTokenInvalid etc.) —
// são falhas de infraestrutura/persistência. Os repositórios convertem falhas
// de domínio específicas em erros do domínio (ex.: row não encontrado em
// GetByKeyID é sinalizado ao caller como nil, *APIToken nil + nil err, para o
// middleware interpretar como ErrTokenInvalid).
// =============================================================================

// ErrNotFound é retornado por queries Get* quando nenhuma linha casa.
// Distinto de "erro real" do DB (conexão, sintaxe). Repositórios convertem
// pgx.ErrNoRows neste erro.
var ErrNotFound = errors.New("postgres: row not found")

// ErrConflict é retornado em write-through quando a condição (ex.: version,
// status) não casa — indica concorrência (lock otimista ou CAS perdido).
var ErrConflict = errors.New("postgres: optimistic/concurrency conflict")

// isNoRows retorna true se err é "no rows in result set" (pgx v5: pgx.ErrNoRows).
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// isUniqueViolation detecta erro de UNIQUE constraint do PostgreSQL (code 23505).
// Implementação leve para não acoplar o pacote a pgconn.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23505") || strings.Contains(err.Error(), "unique constraint")
}

// =============================================================================
// Helpers de conversão pgtype ↔ ponteiros do domínio.
//
// O domínio usa ponteiros Go idiomáticos (*time.Time, *string, *int) para
// representar NULLABLE. O pgx v5 usa tipos pgtype.{Timestamptz,Text,Int4,Int8}
// que carregam um flag Valid. Estes helpers fazem a ponte.
// =============================================================================

// timestamptzToPtr converte pgtype.Timestamptz para *time.Time (nil se NULL).
func timestamptzToPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// textToPtr converte pgtype.Text para *string (nil se NULL).
func textToPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}

// int4ToPtr converte pgtype.Int4 para *int (nil se NULL).
func int4ToPtr(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

// ptrToText converte *string para pgtype.Text (Valid=false se nil).
func ptrToText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// ptrToTimestamptz converte *time.Time para pgtype.Timestamptz.
func ptrToTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
