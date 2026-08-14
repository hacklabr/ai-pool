-- Reverte a issue #19 (plano nas chaves, medições de cota, índice de uso por chave).
DROP INDEX IF EXISTS idx_usage_logs_key_time;
DROP TABLE IF EXISTS key_quota_measurements;
ALTER TABLE provider_keys
    DROP COLUMN IF EXISTS plan_name,
    DROP COLUMN IF EXISTS plan_renewal_date,
    DROP COLUMN IF EXISTS plan_quota_tokens;
