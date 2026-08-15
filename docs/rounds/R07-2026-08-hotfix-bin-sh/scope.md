# Scope of round R07 — hotfix /bin/sh

> Epic: #28 · Variant: Minimal (hotfix da R06) · 2026-08-15

## Variant
minimal

## Requirements introduced
- (nenhum RF novo) Correção do aceite 2 da #27: shell utilizável via caminho absoluto /bin/sh (o que kubelet/kubectl usam).

## Requirements changed
- (nenhum)

## Requirements discontinued
- (nenhum)

## Out of scope for this round
- /bin/bash (removido: busybox não tem applet bash; kubectl cai para /bin/sh naturalmente com ENOENT).

## Métricas de sucesso
kubectl exec -it <pod> -- sh funciona em prod (imagem v0.10.1+).

## Critérios de aceite (espelho da issue #28)
1. docker run --entrypoint /bin/sh -c 'echo ok' imprime ok (caminho absoluto, sem PATH).
2. uid 65532 mantido; binário /aipp intacto.
3. docker exec (estilo kubelet) com /bin/sh funciona em container rodando.
