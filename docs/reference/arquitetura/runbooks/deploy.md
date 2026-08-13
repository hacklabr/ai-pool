# Runbook — Deploy

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

## Propósito

Publicar uma versão do AI Provider Pool (local, staging/CI ou produção via Helm).

## Pré-condições

- `make ci-local` verde (vet + race tests + gofmt + govulncheck + gosec).
- Para produção: KEK (32 B) + pepper (48 B) gerados e guardados em cofre offline;
  Secret `<release>-secrets` materializado no cluster (manual ou via ESO).
- Migrations novas revisadas (par up/down reversível, nunca editar migration aplicada).

## Procedimento

### Local (teste)
1. `cp .env.docker.example .env.docker` (primeira vez) e ajustar.
2. `docker compose up --build` → UI em `:8088/web/login` (admin seedado em DEV_MODE;
   migrations rodam no startup).

### Produção (Helm)
1. Build/push da imagem acontece no CI: push em `main` → tag sha-curto; tag `v*` →
   semver + latest (`ghcr.io/hacklabr/ai-pool`, via OIDC).
2. `make helm-lint` e `make helm-template` (validação local do chart).
3. `make helm-install` (ou o job `deploy-prod` do CI em tags) — `helm upgrade
   --install ... --wait --timeout 5m`.
4. O **migration Job** (hook post-install/post-upgrade) aguarda o banco e roda
   `aipp --migrate-only`; falha no Job marca o release como FAILED.
5. Smoke test: `GET /healthz` → 200; verificar rollout `kubectl rollout status`.
6. Primeira instalação: bootstrap do admin
   (`kubectl exec deploy/<release> -- /aipp --bootstrap-admin-username=...`).

## Rollback deste runbook

Ver `rollback.md`. Upgrade é RollingUpdate `maxSurge:1`/`maxUnavailable:0` +
graceful shutdown — streams de chat não são cortados.

## Pontos de atenção

- Migrations devem ser **backward-compatible** durante rollouts (pods antigos e
  novos convivem).
- Se faltar partição futura de `usage_logs`, inserts caem na partição `DEFAULT`
  (não falham, mas é sinal de alerta) — automação de partições é pendência.
