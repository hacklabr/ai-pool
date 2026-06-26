-- =============================================================================
-- Reverso da migration 202606252100_init_schema.
-- Ordem INVERSA de criação para respeitar FKs.
-- DROP CASCADE em partições para limpar dependências.
-- =============================================================================

-- 11. dead letter
DROP TABLE IF EXISTS usage_logs_dead_letter;

-- 10. audit_log
DROP TABLE IF EXISTS audit_log;

-- 9. rollups
DROP TABLE IF EXISTS usage_rollups;

-- 8. usage_logs (partição default primeiro, depois a particionada, depois partições)
-- Nota: DROP da particionada derruba automaticamente as partições filhas.
DROP TABLE IF EXISTS usage_logs_2026_06;
DROP TABLE IF EXISTS usage_logs_2026_07;
DROP TABLE IF EXISTS usage_logs_2026_08;
DROP TABLE IF EXISTS usage_logs_2026_09;
DROP TABLE IF EXISTS usage_logs_default;
DROP TABLE IF EXISTS usage_logs;

-- 7. rate_limits
DROP TABLE IF EXISTS rate_limits;

-- 6. models
DROP TABLE IF EXISTS models;

-- 5. provider_keys
DROP TABLE IF EXISTS provider_keys;

-- 4. providers
DROP TABLE IF EXISTS providers;

-- 3. api_tokens
DROP TABLE IF EXISTS api_tokens;

-- 2. users
DROP TABLE IF EXISTS users;

-- 1. organizations (por último — referenciada por todas)
DROP TABLE IF EXISTS organizations;

-- Fim do schema. Reaplicar up.sql recria tudo do zero.
