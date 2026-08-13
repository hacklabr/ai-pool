# ADR-0002 — Graceful shutdown no MVP

**Status:** aceito

**Contexto:** O proxy mantém streams SSE de chat com duração de minutos. Sem graceful shutdown, qualquer deploy (RollingUpdate) cortaria streams em andamento, degradando a experiência dos 30 usuários. Custo estimado de ~50 linhas; recomendação unânime (6-0) dos especialistas, puxada do ROADMAP para o MVP.

**Decisão:** Implementar graceful shutdown no MVP: SIGTERM/SIGINT dispara `srv.Shutdown` com timeout configurável (`SHUTDOWN_TIMEOUT`, default 60s), drenando conexões e cancelando jobs de background (scheduler de reativação, rollup, flusher de `last_used_at` com flush final); `preStopSleep` (10s) dá tempo ao Ingress parar de rotear antes do SIGTERM.

**Consequências:** Positivas: deploys com zero-downtime percebido; streams sobrevivem a rollouts; estado dirty do LRU é persistido no shutdown. Negativas: pods podem levar até `ShutdownTimeout` para terminar (impacto em rollouts muito longos); exige disciplina de `context.Context` em todas as goroutines de background.

**Evidências:** `cmd/aipp/main.go:277-307, 699-715`; `internal/config/config.go`; `deploy/helm/ai-pool/values-prod.yaml`.

**Origem:** DECISIONS.md A2 (consenso de especialistas 6-0).
