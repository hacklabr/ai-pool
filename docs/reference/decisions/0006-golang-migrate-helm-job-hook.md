# ADR-0006 — Versionamento de schema com golang-migrate via Helm Job hook

**Status:** aceito

**Contexto:** Schema precisa ser versionado, idempotente e aplicado antes de pods novos receberem tráfego — sem N réplicas competindo para migrar no startup. Em produção, migrations no startup do app são proibidas.

**Decisão:** Migrations SQL versionadas (`migrations/*.up.sql/.down.sql`) embarcadas no binário via `embed.FS`, aplicadas por `golang-migrate` (advisory lock interno). Em produção, um Helm Job hook (`post-install,post-upgrade`) roda `aipp --migrate-only` de forma isolada (falha marca o release FAILED antes de tráfego); em DEV_MODE as migrations rodam no startup.

**Consequências:** Positivas: migrations atômicas e auditáveis; binário único carrega o schema; falha de migration bloqueia o deploy. Negativas: migrations devem ser backward-compatible durante rollouts (pods antigos e novos convivem). Divergência documental registrada: comentários em `main.go` e docs citam hook `pre-install`, mas o hook efetivo é `post-install,post-upgrade` (doc-bug aberto no setup).

**Evidências:** `migrations/`; `internal/adapters/postgres/migrate.go`; `cmd/aipp/main.go` (`--migrate-only`); `deploy/helm/ai-pool/templates/migration-job.yaml`.

**Origem:** DECISIONS.md A6 (consenso de especialistas 6-0).
