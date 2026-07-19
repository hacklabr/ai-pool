-- Adiciona output_limit à tabela models: limite de tokens que o modelo pode
-- gerar em uma única resposta (ex.: 8192). Opcional (nullable) — modelos sem
-- o valor informado usam o default do provedor/cliente.
-- Espelha context_window: mesmo tipo, mesma semântica (nullable, em tokens).
ALTER TABLE models ADD COLUMN output_limit INTEGER;

COMMENT ON COLUMN models.output_limit IS
    'Limite de tokens de saída por resposta (ex.: 8192). Opcional; nullable usa o default do provedor.';
