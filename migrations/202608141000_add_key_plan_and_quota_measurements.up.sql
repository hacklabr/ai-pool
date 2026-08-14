-- Issue #19: registrar plano nas chaves e auditar ativações/desativações.
--
-- 1) provider_keys ganha os campos de plano (RF-01):
--    plan_name, plan_renewal_date (âncora do ciclo), plan_quota_tokens
--    (denominador configurado pelo admin). Nullable — chaves pay-as-you-go
--    não têm ciclo de plano.
-- 2) Nova tabela key_quota_measurements (RF-05): a cada desativação por
--    teto, soma de tokens da janela persistida como medição observada da
--    cota (piso conservador), refinando o denominador ao longo dos ciclos.
--    Sem FK física — segue a convenção de FK LÓGICA (como usage_logs).
-- 3) Índice em usage_logs(key_id, created_at) para a soma local de tokens
--    por chave/janela (RF-02). usage_logs é PARTITION BY RANGE; no PG16 o
--    CREATE INDEX no pai propaga para todas as partições existentes (e
--    futuras, via default partition).

ALTER TABLE provider_keys
    ADD COLUMN plan_name          TEXT,
    ADD COLUMN plan_renewal_date  DATE,
    ADD COLUMN plan_quota_tokens  BIGINT;

COMMENT ON COLUMN provider_keys.plan_name IS 'Nome do plano da assinatura (ex.: GLM Coding Lite, Kimi Code). Nullable: pay-as-you-go não tem ciclo.';
COMMENT ON COLUMN provider_keys.plan_renewal_date IS 'Dia do mês em que o ciclo renova (âncora do ciclo semanal/mensal). Nullable.';
COMMENT ON COLUMN provider_keys.plan_quota_tokens IS 'Denominador configurado pelo admin (tokens por ciclo). Usado no % consumido (issue #20).';

CREATE TABLE key_quota_measurements (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT 1,
    key_id          BIGINT NOT NULL,                 -- FK LÓGICA (sem constraint, como usage_logs)
    limit_layer     TEXT NOT NULL
                    CHECK (limit_layer IN ('window_5h','weekly','monthly','balance','tier','other')),
    tokens          BIGINT NOT NULL,                 -- soma observada na janela (piso conservador)
    window_start    TIMESTAMPTZ NOT NULL,
    window_end      TIMESTAMPTZ NOT NULL,
    error_message   TEXT,                            -- mensagem upstream SANITIZADA (≤200 chars, sem segredo)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_key_quota_measurements_key_time
    ON key_quota_measurements (key_id, created_at);

COMMENT ON TABLE key_quota_measurements IS 'Medições observadas de cota por chave: soma de tokens da janela no momento da desativação por teto (RF-05).';
COMMENT ON COLUMN key_quota_measurements.limit_layer IS 'Camada do limite que desativou a chave: window_5h|weekly|monthly|balance|tier|other.';

-- Soma de tokens por chave/janela (RF-02): cascata para as partições no PG16.
CREATE INDEX idx_usage_logs_key_time ON usage_logs (key_id, created_at);
