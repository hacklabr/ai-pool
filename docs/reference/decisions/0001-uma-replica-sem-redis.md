# ADR-0001 — Uma réplica em produção, sem Redis

**Status:** aceito

**Contexto:** O pool atende ~30 usuários internos sem SLA formal. Múltiplas réplicas exigiriam estado compartilhado (LRU, afinidade, rate limit, sessões), introduzindo bugs de estado distribuído e a dependência operacional de Redis. A alternativa (Opção B: multi-réplica + Redis) foi avaliada e adiada para o ROADMAP, com gatilhos explícitos (SLA duro, RPS > 50/s).

**Decisão:** Rodar 1 réplica em produção, sem Redis. Todas as stores são abstraídas por interfaces (`LRUStore`, `AffinityStore`, `RateLimiter`, `SessionStore`) com apenas implementações in-memory no MVP; `go-redis` não é importado. Zero-downtime em deploys é obtido via RollingUpdate `maxSurge:1`/`maxUnavailable:0` + graceful shutdown (ver ADR-0002).

**Consequências:** Positivas: simplicidade operacional, sem classe inteira de bugs de estado distribuído, upgrades de store não exigem migração de dados. Negativas: restart do pod zera estado in-memory (mitigado por boot seeding do LRU a partir de `last_used_at` e reativação lazy); ponto único de falha entre deploys (mitigado por maxSurge). Subir para ≥2 réplicas torna Redis mandatório e exige novas implementações das interfaces — sem mudança no domínio.

**Evidências:** `go.mod`; `internal/domain/stores.go`; `internal/adapters/store/{lru,affinity,ratelimit,session}_memory.go`; `deploy/helm/ai-pool/values-prod.yaml`; `deploy/helm/ai-pool/templates/deployment.yaml`; `cmd/aipp/main.go` (boot seeding).

**Origem:** DECISIONS.md A1 (consenso de especialistas 6-0).
