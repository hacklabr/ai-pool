# ADR-0008 — Tokens de API com HMAC-SHA256 + pepper

**Status:** aceito

**Contexto:** Tokens de API persistidos precisam sobreviver a um dump do banco sem se tornarem utilizáveis. SHA-256 puro é vulnerável a rainbow/brute-force se a entropia fosse baixa; Argon2/bcrypt (hash lento) é desnecessário para segredos de alta entropia e adicionaria latência no hot path de cada request.

**Decisão:** Formato `sk-aipp-<key_id(8)>-<secret(48)>`: `key_id` é público e indexado (lookup O(1) via partial unique index); apenas `HMAC-SHA256(secret, pepper)` é persistido (`token_hmac` BYTEA); o pepper é externo ao DB (arquivo readOnly via Secret). Comparação em tempo constante (`subtle.ConstantTimeCompare`). Revogação é soft (`revoked_at`), sem DELETE físico.

**Consequências:** Positivas: dump do DB não compromete tokens (defesa em profundidade); lookup sub-ms; verificação sem hash lento. Negativas: rotação de pepper exige re-emissão de tokens (não há versionamento de pepper como há de KEK); pepper vira segredo crítico adicional a gerenciar.

**Evidências:** `internal/domain/token.go`; `internal/adapters/postgres/token_validator.go`; `migrations/202606252100_init_schema.up.sql` (`api_tokens`); `internal/config/config.go`.

**Origem:** DECISIONS.md A8 (consenso de especialistas 6-0).
