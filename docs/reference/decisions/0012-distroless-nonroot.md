# ADR-0012 — Imagem base distroless/base-debian12:nonroot

**Status:** aceito

**Contexto:** A imagem de runtime deve ter superfície mínima (sem shell, sem package manager) e rodar como não-root. `distroless/static` não contém CA roots — e o proxy precisa validar TLS de provedores externos (HTTPS), portanto `static` não serve.

**Decisão:** Runtime em `gcr.io/distroless/base-debian12:nonroot` (CA roots em `/etc/ssl/certs`, UID/GID 65532 pré-configurado). Build multi-stage com `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w"`; migrations e assets web embarcados via `embed.FS` (sem arquivos soltos); `TZ=UTC`.

**Consequências:** Positivas: `runAsNonRoot` sem gestão manual de UID; sem shell para pivot pós-exploração; imagem pequena. Negativas: debug em produção exige ephemeral containers (sem `kubectl exec sh`); glibc do debian12 acompanha CVEs de OS (mitigado por scan trivy no CI); build reproduzível depende de pin de versão do Go no builder.

**Evidências:** `Dockerfile`; `.github/workflows/ci.yml` (trivy).

**Origem:** DECISIONS.md A12 (consenso de especialistas 6-0).
