-- Adiciona reasoning_options à tabela models: valores de reasoning effort
-- suportados pelo modelo (ex.: ["low","high","max"]) — as "variações" que o
-- OpenCode expõe como variants (reasoningEffort). Fonte: models.dev
-- (ADR-0017). Nullable/vazio = modelo sem seletor de effort.
ALTER TABLE models ADD COLUMN reasoning_options JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN models.reasoning_options IS
    'Valores de reasoning effort suportados (ex.: ["low","high","max"]). Vazio = sem seletor de effort. Fonte: models.dev (ADR-0017).';
