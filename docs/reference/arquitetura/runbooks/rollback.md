# Runbook — Rollback

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

## Propósito

Reverter um release com falha ou regressão.

## Pré-condições

- Identificar a revisão anterior saudável: `helm history <release>`.

## Procedimento

1. `helm rollback <release> <revisão>` — o migration Job hook **não** re-roda em
   rollback; avaliar se o schema atual é compatível com a versão revertida
   (migrations são desenhadas para serem backward-compatible; se o release
   revertido incluiu migration destrutiva, tratar como incidente — ver
   `incidentes.md`).
2. Verificar `kubectl rollout status deploy/<release>`.
3. Smoke test: `GET /healthz`; testar uma completion real via proxy.

## Rollback deste runbook

Refazer o deploy da versão alvo (forward-fix é preferível a rollback quando a
migration já aplicou mudanças destrutivas).

## Pontos de atenção

- `helm uninstall` preserva StatefulSet/PVC do PostgreSQL (`resource-policy:
  keep`) — dados sobrevivem a uninstall; reset exige deleção explícita.
- Estado in-memory (sessões, LRU, rate-limit) se perde no rollback: usuários
  admin precisam re-logar.
