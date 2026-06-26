// Package domain contém a lógica de negócio pura do AI Provider Pool.
//
// Este pacote (e seus subpacotes) NÃO depende de frameworks de transporte
// (Beego, net/http) nem de infraestrutura (PostgreSQL, Redis). Define apenas
// entidades, value objects, serviços de domínio e PORTS (interfaces) que os
// adapters implementam. Esta separação segue a arquitetura hexagonal
// (Ports & Adapters) aprovada em consenso:
//
//   - Proxy Core:    net/http puro (fora do Beego) com http.Flusher para SSE.
//   - Admin/UI:      Beego controllers + partials HTMX.
//   - Persistência:  PostgreSQL via pgx v5.
//   - Estado:        memoryStore (LRU, afinidade, rate-limit). Sem Redis no MVP.
//
// Decisões consolidadas da especificação (spec-388d67e6.md §3):
//   - 1 réplica em produção (Opção A); store abstrações prontas para Redis futuro.
//   - organization_id NOT NULL DEFAULT 1 em todas as tabelas (multi-tenant ready).
//   - Tokens sk-aipp-... com HMAC-SHA256+pepper; DB hit por request (sem cache).
//   - Chaves de provedores com envelope encryption (AES-256-GCM + KEK externo).
//   - Streaming SSE com 3 timeouts e cancelamento propagado ao upstream.
package domain
