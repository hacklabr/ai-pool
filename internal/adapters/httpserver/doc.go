// Package httpserver agrupa os adapters de transporte HTTP do AI Provider Pool.
//
// Separação fundamental (spec §3.1.2):
//   - proxy: net/http PURO com http.Flusher para streaming SSE (NÃO usa Beego
//     para evitar buffering no hot path). Rotas sob /v1/* (OpenAI-compatible).
//   - admin: Beego controllers para a UI administrativa (CRUD + dashboard).
//     Partials HTMX + JSON isolado para gráficos. Sessão httpOnly + CSRF.
//
// Ambos rodam no MESMO binário/processo, expostos pelo mesmo http.Server mas
// em muxes distintos para evitar acoplamento.
package httpserver
