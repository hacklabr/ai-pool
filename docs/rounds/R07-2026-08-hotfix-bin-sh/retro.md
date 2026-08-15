# Retro of round R07 — hotfix /bin/sh

## Round signals
- **Specification gaps:** 1 — o critério de aceite da R06 testou o shell por PATH (--entrypoint sh) e não pelo caminho absoluto que o kubelet usa; o bug passou pelo gate.
- **New requirements discovered in Stage 3:** 0.
- **Late infeasibilities:** 0.
- **Documentation contradictions (`doc-bug`):** 0.
- **Registered overrides:** 0.
- **Feedback:** incidente em prod reportado pelo autor.

## Process learnings
- Aceite de infra deve testar o CAMINHO DE CONSUMO real (kubectl exec /bin/sh), não o mecanismo interno (PATH). Registro: verificação por caminho de uso em critérios de aceite de imagem/infra.
- Docker COPY de symlink individual desreferencia o alvo; copiar o DIRETÓRIO com links RELATIVOS preserva. Documentado no Dockerfile.
