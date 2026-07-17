# syntax=docker/dockerfile:1.7
# =============================================================================
# Dockerfile multi-stage — AI Provider Pool (Go + Beego + net/http + PostgreSQL)
#
# Estratégia (DevOps spec §3.5.1):
#   Stage 1 (builder): golang:<ver>-alpine.
#   Stage 2 (runtime): gcr.io/distroless/base-debian12:nonroot
#     - CA roots presentes (base-debian12, não static) — HTTPS para provedores.
#     - UID/GID 65532 pré-configurado (nonroot) — runAsNonRoot sem UID manual.
#     - Sem shell, sem package manager — superfície de ataque mínima.
#
# O binário embarca migrations (migrations/embed.go) e assets web
# (web/embed.go) via //go:embed — não há arquivos soltos a copiar.
#
# Build:
#   docker build -t ai-provider-pool:dev .
#   docker build -t ai-provider-pool:1.0.0 --build-arg VERSION=1.0.0 .
#
# BuildKit (opcional, acelera rebuilds): se buildx estiver disponível,
# rode `DOCKER_BUILDKIT=1 docker build ...`. As diretivas `--mount=type=cache`
# são aplicadas automaticamente quando BuildKit está ativo; em modo legacy
# são ignoradas silenciosamente (build funciona sem elas).
# =============================================================================

# === Stage 1: Builder ===
# Versão pinada conforme go.mod (go 1.26.5).
FROM golang:1.26.5-alpine AS builder

# git é necessário para operações de VCS no toolchain Go; ca-certificates
# para HTTPS em `go mod download`.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache de módulos: copia go.mod/go.sum ANTES do resto do código para que
# `go mod download` seja cacheado entre builds (camada Docker estável — só
# invalida quando deps mudam). Esta estratégia independe de BuildKit.
COPY go.mod go.sum ./
RUN go mod download
# Copia o restante do código-fonte.
COPY . .

# Argumentos de build opcionais para injeção de versão no binário.
# (Não usados no main.go ainda; placeholder para F11 — CI/CD.)
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Build do binário ESTÁTICO:
#   - CGO_ENABLED=0: binário sem dependência de libc (compatível com distroless).
#   - GOOS=linux: target Linux (multi-arch via buildx se necessário).
#   - -ldflags "-s -w": remove símbolos de debug e DWARF (imagem menor).
#   - -trimpath: remove paths absolutos do binário (reprodutibilidade).
#   - -buildvcs=false: não embute info do git (Docker context não é repo).
#
# Nota sobre cache de build: em Docker com BuildKit habilitado
# (DOCKER_BUILDKIT=1 + buildx instalado), pode-se adicionar:
#     --mount=type=cache,target=/go/pkg/mod
#     --mount=type=cache,target=/root/.cache/go-build
# para acelerar rebuilds. Em modo legacy (sem BuildKit), estas diretivas
# falham — por isso NÃO as usamos aqui. O cache de `go mod download`
# (acima) já cobre a maior parte do ganho entre rebuilds.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -trimpath \
      -buildvcs=false \
      -o /out/aipp \
      ./cmd/aipp

# Verificação de sanidade: binário deve existir.
RUN test -f /out/aipp && ls -lh /out/aipp

# === Stage 2: Runtime (distroless) ===
# base-debian12 (NÃO static): traz CA certificates em /etc/ssl/certs.
# Essencial — o proxy fala HTTPS com provedores (Gemini, Mistral, DeepSeek,
# Z.ai, Kimi). distroless/static não tem CA roots e quebraria TLS verify.
FROM gcr.io/distroless/base-debian12:nonroot

# TZinfo incluso no base-debian12. Definimos UTC como padrão (timestamps em
# logs sempre em UTC, consistente com spec DBA §1).
ENV TZ=UTC

WORKDIR /

# Copia o binário do builder.
COPY --from=builder /out/aipp /aipp

# Porto padrão (config.go default HTTP_ADDR=:8080). EXPOSE é documental;
# o publish é feito pelo `docker run -p` ou compose `ports:`.
EXPOSE 8080

# distroless nonroot já define USER nonroot (UID/GID 65532). Redundante mas
# explícito para auditoria de securityContext.
USER nonroot

# Sem shell no distroless — ENTRYPOINT deve ser array (exec form).
# Args passados após a imagem sobrescrevem (ENTRYPOINT exec form mantém).
ENTRYPOINT ["/aipp"]
