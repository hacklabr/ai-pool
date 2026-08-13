# ADR-0003 — Arquitetura Hexagonal (Ports & Adapters)

**Status:** aceito

**Contexto:** O sistema precisa trocar implementações de infraestrutura sem tocar regras de negócio (ex.: memoryStore → Redis no ROADMAP; IdentityDecryptor → AES-GCM por ambiente) e manter o domínio testável sem DB/HTTP. Frameworks web e drivers de DB são tratados como detalhes.

**Decisão:** Adotar Arquitetura Hexagonal: `internal/domain/` é puro (entidades + interfaces/ports: `TokenValidator`, `KeyResolver`, `LRUStore`, `AffinityStore`, `SessionStore`, `KeyDecryptor/KeyEncryptor`, `UsageCollector`); `internal/adapters/` contém as implementações (postgres, store in-memory, crypto, httpserver, security); composição (wiring) exclusivamente em `cmd/aipp/main.go`. Beego é detalhe de transporte, não fundação.

**Consequências:** Positivas: domínio testável com mocks (ports mínimas por consumidor, ex.: `RollupJobPort`); troca de adapters sem refactor de negócio; stubs deny-by-default em dev sem DB. Negativas: mais indireção (adapters como `EngineDepsAdapter`/`SchedulerDepsAdapter`); exige disciplina para não vazar tipos de infra (pgx, http) para o domínio.

**Evidências:** `internal/domain/` (`stores.go`, `routing.go`, `keyresolver.go`); `internal/adapters/{postgres,store,crypto,httpserver,security}/`; `cmd/aipp/main.go` (composição).

**Origem:** DECISIONS.md A3 (consenso de especialistas 6-0).
