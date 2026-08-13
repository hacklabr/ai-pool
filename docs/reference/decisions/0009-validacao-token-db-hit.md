# ADR-0009 — Validação de token com DB hit por request (sem cache)

**Status:** aceito

**Contexto:** Cache positivo de tokens criaria janela de revogação (token revogado continuaria válido até o TTL do cache). A alternativa (cache com invalidação) reintroduz complexidade de consistência. A latência do lookup (partial index, sub-ms) é ruído frente à latência de uma chamada LLM (segundos).

**Decisão:** Toda request ao proxy valida o token contra o banco: lookup por `key_id` → HMAC constant-time → checagem de status do usuário (revogação imediata) → expiração. Não há cache positivo. Respostas ao cliente são 401 genéricos (anti-enumeration), com motivo apenas em log interno.

**Consequências:** Positivas: revogação instantânea e determinística; desabilitar usuário bloqueia no próximo request; sem estado de cache para sincronizar. Negativas: 2 queries por request no hot path (aceito: volume baixo); indisponibilidade do DB derruba o proxy inteiro (fail-closed, desejado).

**Evidências:** `internal/adapters/postgres/token_validator.go`; `migrations/202606252100_init_schema.up.sql` (partial index).

**Origem:** DECISIONS.md A9 (consenso de especialistas 6-0).
