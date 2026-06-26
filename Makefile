# Makefile do AI Provider Pool.
#
# Targets comuns para desenvolvimento. Para produção, o deploy é via Helm
# (ver deploy/helm/ em F10) e as migrations rodam via Job hook.

GO ?= go

.PHONY: help build run test vet fmt tidy \
        migrate-up migrate-down migrate-version migrate-new \
        docker-build \
        helm-lint helm-template helm-install \
        ci-local

help: ## Mostra os targets disponíveis.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Compila o binário para ./bin/aipp.
	$(GO) build -o ./bin/aipp ./cmd/aipp

run: build ## Roda o servidor local (requer .env configurado).
	./bin/aipp

test: ## Roda os testes com race detector.
	$(GO) test -race -cover ./...

vet: ## Roda go vet.
	$(GO) vet ./...

fmt: ## Formata o código Go.
	$(GO) fmt ./...

tidy: ## Atualiza go.mod/go.sum.
	$(GO) mod tidy

# === Migrations (golang-migrate via CLI) ===
# Requer DATABASE_URL ou DB_DSN exportada.
# Instalar o CLI: go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

migrate-up: ## Aplica migrations pendentes (requer DATABASE_URL).
	./scripts/migrate.sh up

migrate-down: ## Reverte a última migration (requer DATABASE_URL).
	./scripts/migrate.sh down

migrate-version: ## Mostra a versão atual do schema.
	./scripts/migrate.sh version

migrate-new: ## Cria esqueleto de migration. Uso: make migrate-new name=adiciona_indice
	@test -n "$(name)" || { echo "Uso: make migrate-new name=slug_descritivo"; exit 1; }
	./scripts/migrate.sh new "$(name)"

# === Docker ===
docker-build: ## Builda a imagem Docker (dev). Produção usa o Helm/Dockerfile multi-stage.
	docker build -t ai-provider-pool:dev .

# === Helm (F10 — Produção K8s) ===
HELM_CHART ?= deploy/helm/ai-pool
HELM_RELEASE ?= ai-pool
HELM_NAMESPACE ?= default

helm-lint: ## Valida o chart Helm (helm lint).
	helm lint $(HELM_CHART)

helm-template: ## Renderiza os manifests sem aplicar (debug). Use HELM_SETS para overrides.
	helm template $(HELM_RELEASE) $(HELM_CHART) \
		--set database.existingSecret=ai-pool-db-prod \
		$(HELM_SETS)

helm-install: ## Instala/atualiza o release no cluster (helm upgrade --install).
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
		--namespace $(HELM_NAMESPACE) --create-namespace \
		--wait --timeout 5m

# === CI local (F11) ===
# Roda localmente os mesmos gates do GitHub Actions (exceto Docker/Trivy/Helm
# que exigem ferramentas específicas — use os targets separados se quiser).
# Use antes de push para evitar CI vermelho.
ci-local: ## Roda vet + race tests + gosec + govulncheck + gofmt check (todas as gates do CI).
	@echo "==> go vet"
	$(GO) vet ./...
	@echo "==> go test -race"
	$(GO) test -race -count=1 ./...
	@echo "==> gofmt check"
	@fmt_out=$$(gofmt -l .); \
	if [ -n "$$fmt_out" ]; then \
		echo "FAIL: gofmt divergences in:"; echo "$$fmt_out"; exit 1; \
	fi
	@echo "==> govulncheck"
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "WARN: govulncheck não instalado — pulando (instale: go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	} && govulncheck ./...
	@echo "==> gosec"
	@command -v gosec >/dev/null 2>&1 || { \
		echo "WARN: gosec não instalado — pulando (instale: go install github.com/securego/gosec/v2/cmd/gosec@latest)"; \
	} && gosec -severity medium -exclude-dir=test ./...
	@echo "==> ci-local: ALL OK"
