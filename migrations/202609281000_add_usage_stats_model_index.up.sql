-- Painel de Estatísticas de Utilização (admin, /web/admin/usage):
-- agrega usage_logs com filtro opcional por modelo. Sem índice começando por
-- (organization_id, model), o filtro combinado modelo+período varre partições
-- inteiras. key_id já é coberto por idx_usage_logs_key_time (migration
-- 202608141000). usage_logs é PARTITION BY RANGE: no PG16 o CREATE INDEX no
-- pai propaga para todas as partições existentes (e futuras via default).

-- Filtro por modelo + janela temporal (painel de estatísticas, breakdown model).
CREATE INDEX idx_usage_logs_model_time
    ON usage_logs (organization_id, model, created_at DESC);
