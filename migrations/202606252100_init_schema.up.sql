-- =============================================================================
-- Migration 202606252100: Schema inicial do AI Provider Pool (~11 entidades).
--
-- Especificação autoritativa: .mesa/specifications/spec-388d67e6.md §3.4
-- Convenções (spec §3.4.1):
--   * IDs: BIGINT GENERATED ALWAYS AS IDENTITY (uniforme, join-friendly).
--   * Timestamps: TIMESTAMPTZ NOT NULL DEFAULT now() (UTC sempre).
--   * organization_id: BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id)
--     em TODAS as tabelas multitenant (pavimenta multi-tenancy, spec §3.1.4).
--   * Toda FK recebe índice.
--   * Índices composite começam por organization_id (queries sempre filtram).
--
-- Reversível (ver .down.sql). Sem DDL destrutivo em up.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Extensões
-- -----------------------------------------------------------------------------
-- Nenhuma extensão é necessária no MVP. gen_random_uuid() já é nativo no PG 13+.
-- pg_partman NÃO é usado (pré-criação de partições via CronJob/aplicação).

-- -----------------------------------------------------------------------------
-- 1. organizations (tenant). Seed id=1 = organização default do MVP.
-- -----------------------------------------------------------------------------
CREATE TABLE organizations (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    status     TEXT NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  organizations IS 'Tenant/organização. MVP usa apenas id=1 (default). Multi-tenancy real no roadmap.';
COMMENT ON COLUMN organizations.slug IS 'Identificador amigável, único. Ex.: default.';

-- Seed da organização default (referenciada por DEFAULT 1 em todas as tabelas).
-- NÃO especificamos id: a sequência IDENTITY começa em 1, então o primeiro INSERT
-- gera id=1 naturalmente. Isto evita o conflito do GENERATED ALWAYS (que proíbe
-- INSERT explícito de id) e dispensa setval. Se um dia precisarmos reseedar com
-- id específico, usar OVERRIDING SYSTEM VALUE + setval.
INSERT INTO organizations (name, slug) VALUES ('Default Organization', 'default');

-- -----------------------------------------------------------------------------
-- 2. users
--    Senha em password_hash (Argon2id encoded string, spec §3.3.5).
--    status='disabled' + disabled_at != NULL bloqueia acesso ao pool (revogação
--    imediata validada no middleware de auth, spec §3.3.3).
--    NÃO há seed de admin nesta migration — credenciais NUNCA em SQL versionado
--    (princípio de segurança). O admin inicial é criado via CLI:
--        aipp admin create  (fase F4)
-- -----------------------------------------------------------------------------
CREATE TABLE users (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id    BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    username           TEXT NOT NULL,
    password_hash      TEXT NOT NULL,                       -- Argon2id encoded
    password_algo      TEXT NOT NULL DEFAULT 'argon2id',
    role               TEXT NOT NULL DEFAULT 'user'
                           CHECK (role IN ('admin', 'user')),
    status             TEXT NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'disabled')),
    failed_login_count INTEGER NOT NULL DEFAULT 0,
    last_login_at      TIMESTAMPTZ,
    disabled_at        TIMESTAMPTZ,                         -- não-NULL quando status='disabled'
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_org_username_unique UNIQUE (organization_id, username)
);

CREATE INDEX idx_users_organization_role ON users (organization_id, role);
CREATE INDEX idx_users_status            ON users (organization_id, status);

COMMENT ON TABLE  users IS 'Usuários do pool (admin ou usuário comum). Soft-disable via status+disabled_at (sem DELETE físico).';
COMMENT ON COLUMN users.password_hash IS 'Argon2id encoded string. NUNCA plaintext, NUNCA logado.';

-- -----------------------------------------------------------------------------
-- 3. api_tokens (tokens sk-aipp-... dos usuários)
--    Formato (spec §3.3.3): sk-aipp-[key_id 8 chars]-[secret 48 chars]
--    * key_id  -> parte pública, lookup O(1) por índice UNIQUE parcial.
--    * token_hmac -> HMAC-SHA256(secret, pepper externo). Defesa em profundidade
--      contra dump do DB (spec §3.3.3, D3 do Security Architect).
--    * pepper vive em KEK/External Secrets (NUNCA no DB).
--    O valor completo (sk-aipp-...) só é conhecido na geração. Soft-revoke via
--    revoked_at (sem DELETE físico — preserva histórico para auditoria).
-- -----------------------------------------------------------------------------
CREATE TABLE api_tokens (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id),
    organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    key_id         CHAR(8) NOT NULL,                -- parte pública, ex.: "AB12CD34"
    token_hmac     BYTEA NOT NULL,                  -- HMAC-SHA256(secret, pepper)
    name           TEXT NOT NULL DEFAULT '',        -- rótulo humano ("meu-token-dev")
    status         TEXT NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'revoked', 'expired')),
    last_used_at   TIMESTAMPTZ,
    last_used_ip   INET,
    expires_at     TIMESTAMPTZ,                     -- NULL = sem expiração (desencorajado)
    revoked_at     TIMESTAMPTZ,                     -- não-NULL quando revogado
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
    -- NOTA: a unicidade parcial de key_id (apenas entre tokens NÃO revogados)
    -- é garantida por partial UNIQUE INDEX abaixo. PostgreSQL NÃO suporta
    -- partial UNIQUE CONSTRAINT inline (WHERE) em CREATE TABLE — apenas
    -- CREATE [UNIQUE] INDEX ... WHERE. O índice parcial atende igualmente.
);

-- Unicidade parcial: um mesmo key_id não pode existir em 2 tokens vivos
-- (revoked_at IS NULL). Permite reuso do key_id após revogação (se desejado).
CREATE UNIQUE INDEX api_tokens_active_key_unique
    ON api_tokens (key_id)
    WHERE revoked_at IS NULL;

-- Lookup O(1) por key_id (apenas tokens ativos). Este é o índice do hot path.
CREATE INDEX idx_api_tokens_active_lookup
    ON api_tokens (key_id)
    WHERE revoked_at IS NULL AND status = 'active';

CREATE INDEX idx_api_tokens_user ON api_tokens (organization_id, user_id);

COMMENT ON TABLE  api_tokens IS 'Tokens sk-aipp-... dos usuários. HMAC+pepper. Lookup O(1) por key_id. Soft-revoke.';
COMMENT ON COLUMN api_tokens.token_hmac IS 'HMAC-SHA256(secret, pepper). Pepper NUNCA no DB.';

-- -----------------------------------------------------------------------------
-- 4. providers (provedores de LLM — Gemini, Mistral, DeepSeek, Z.ai, Kimi).
--    Todos consumidos via schema OpenAI-compatible (spec §3.2.1). api_spec
--    pavimenta parsers de erro específicos por provedor e roteamento "auto".
-- -----------------------------------------------------------------------------
CREATE TABLE providers (
    id                          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id             BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    name                        TEXT NOT NULL,
    slug                        TEXT NOT NULL,
    base_url                    TEXT NOT NULL,
    api_spec                    TEXT NOT NULL DEFAULT 'openai',  -- 'openai' | 'gemini-native' ...
    default_user_agent_override TEXT,                            -- override global do provedor
    is_active                   BOOLEAN NOT NULL DEFAULT true,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT providers_org_slug_unique UNIQUE (organization_id, slug)
);

COMMENT ON TABLE  providers IS 'Provedores de LLM. base_url com SSRF guard aplicado na app (spec §3.3.6).';
COMMENT ON COLUMN providers.api_spec IS 'Dialeto da API. Pavimenta ErrorParser registry e roteamento futuro.';

-- -----------------------------------------------------------------------------
-- 5. provider_keys (chaves de API dos provedores — ativo MAIS sensível).
--    encrypted_value: AES-256-GCM ciphertext (envelope encryption, spec §3.3.2).
--    KEK NUNCA no DB. encryption_key_id suporta rotação de KEK.
--    Máquina de estados: enabled | auto_disabled | manual_disabled (spec §3.2.4).
--    disabled_until: não-NULL quando status='auto_disabled' (reativação agendada).
--    version: lock otimista para config admin (spec §3.4.4).
-- -----------------------------------------------------------------------------
CREATE TABLE provider_keys (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id           BIGINT NOT NULL REFERENCES providers(id),
    organization_id       BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    label                 TEXT NOT NULL,                       -- rótulo humano
    encrypted_value       BYTEA NOT NULL,                      -- AES-256-GCM ciphertext
    encryption_key_id     TEXT NOT NULL DEFAULT 'v1',          -- suporta rotação de KEK
    status                TEXT NOT NULL DEFAULT 'enabled'
                              CHECK (status IN ('enabled', 'auto_disabled', 'manual_disabled')),
    disabled_reason       TEXT
                              CHECK (disabled_reason IS NULL
                                  OR disabled_reason IN ('manual','quota_exceeded',
                                                          'rate_limited','error_threshold',
                                                          'auth_failed','parse_failed_retry')),
    disabled_at           TIMESTAMPTZ,
    disabled_until        TIMESTAMPTZ,                          -- scheduler lê este campo
    error_count           INTEGER NOT NULL DEFAULT 0,           -- janela deslizante
    error_window_started_at TIMESTAMPTZ,
    consecutive_errors    INTEGER NOT NULL DEFAULT 0,           -- reset em sucesso
    last_error_at         TIMESTAMPTZ,
    last_error_hint       TEXT,                                 -- data extraída do parse
    last_used_at          TIMESTAMPTZ,                          -- LRU (heurística, debounce 60s)
    user_agent_override   TEXT,                                 -- override por chave
    version               INTEGER NOT NULL DEFAULT 1,           -- lock otimista (spec §3.4.4)
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seleção LRU: chaves ativas do provedor, ordenadas por last_used_at.
-- NULLS FIRST garante que chaves nunca usadas (cold-start) vêm primeiro.
CREATE INDEX idx_provider_keys_lru
    ON provider_keys (organization_id, provider_id, status, last_used_at NULLS FIRST);

-- Scheduler de reativação: consulta "quais chaves reativar agora".
-- Partial index — consulta fica minúscula (spec §3.4.3).
CREATE INDEX idx_provider_keys_reactivate
    ON provider_keys (organization_id, disabled_until)
    WHERE status = 'auto_disabled' AND disabled_until IS NOT NULL;

COMMENT ON TABLE  provider_keys IS 'Chaves de provedores. AES-256-GCM envelope. KEK nunca no DB.';
COMMENT ON COLUMN provider_keys.encrypted_value IS 'AES-256-GCM ciphertext. DEK por chave, KEK master externo.';
COMMENT ON COLUMN provider_keys.encryption_key_id IS 'Versão da KEK usada. Permite rotação gradual.';
COMMENT ON COLUMN provider_keys.disabled_until IS 'Não-NULL quando status=auto_disabled. Scheduler reativa quando <= now().';

-- -----------------------------------------------------------------------------
-- 6. models (modelos de LLM oferecidos por cada provedor).
--    model_aliases (JSONB): aliasing simples para roadmap (ex.: gpt-4 -> interno).
--    model_pricing (JSONB): custo nullable para roadmap {input, output, cached}.
--    context_window: pavimenta failover cross-provider (janela pode variar).
-- -----------------------------------------------------------------------------
CREATE TABLE models (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id     BIGINT NOT NULL REFERENCES providers(id),
    organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    name            TEXT NOT NULL,                    -- nome real no provedor
    display_name    TEXT,
    context_window  INTEGER,                          -- tokens (pavimenta cross-provider)
    model_aliases   JSONB NOT NULL DEFAULT '[]',      -- ['gpt-4', 'gpt4'] etc.
    model_pricing   JSONB,                            -- {"input": 0.001, "output": 0.002, "cached": 0.0001}
    modalities      JSONB NOT NULL DEFAULT '[]',      -- ['text','image'] etc.
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT models_provider_name_unique UNIQUE (provider_id, name)
);

CREATE INDEX idx_models_org_active ON models (organization_id, is_active);

COMMENT ON TABLE  models IS 'Modelos de LLM. model_aliases/model_pricing como JSONB (simples p/ MVP, tabela dedicada se crescer).';

-- -----------------------------------------------------------------------------
-- 7. rate_limits (CONFIGURAÇÃO de rate limiting).
--    ATENÇÃO: a CONFIGURAÇÃO mora aqui; a CONTAGEM (janela deslizante) fica em
--    store in-memory (golang.org/x/time/rate) no hot path, nunca no DB por
--    request (write amplification). Spec §3.2.5.
-- -----------------------------------------------------------------------------
CREATE TABLE rate_limits (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    scope_type      TEXT NOT NULL
                        CHECK (scope_type IN ('global','organization','user','token')),
    scope_id        BIGINT,                            -- NULL p/ global/org; id p/ user/token
    limit_value     INTEGER NOT NULL,                  -- ex.: req permitidas na janela
    window_seconds  INTEGER NOT NULL,                  -- ex.: 60 (por minuto)
    is_active       BOOLEAN NOT NULL DEFAULT true,
    version         INTEGER NOT NULL DEFAULT 1,        -- lock otimista
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT rate_limits_scope_unique
        UNIQUE (organization_id, scope_type, scope_id)
);

COMMENT ON TABLE  rate_limits IS 'Configuração de rate limit. Runtime fica em store in-memory (spec §3.2.5).';

-- -----------------------------------------------------------------------------
-- 8. usage_logs — PARTICIONADA MENSAL (alto volume, spec §3.4.2).
--
--    IMPORTANTE: tabela particionada NÃO admite FK física em PostgreSQL (até
--    v18; v19 trouxe melhorias parciais). As referências a users/tokens/providers
--    /provider_keys são FK LÓGICAS (sem CONSTRAINT), garantidas pela aplicação.
--    Soft-delete em users/api_tokens preserva a integridade referencial histórica.
--
--    PK composta (id, created_at) é exigência do PG em particionamento RANGE:
--    a partição deve ser determinável pela PK.
--
--    Retention: DROP de partição após 90 dias (granulares). Agregados em
--    usage_rollups ficam por anos (spec §3.4.2).
-- -----------------------------------------------------------------------------
CREATE TABLE usage_logs (
    id               BIGINT GENERATED ALWAYS AS IDENTITY,
    organization_id  BIGINT NOT NULL DEFAULT 1,
    user_id          BIGINT NOT NULL,
    token_id         BIGINT NOT NULL,
    provider_id      BIGINT NOT NULL,
    key_id           BIGINT NOT NULL,
    model            TEXT NOT NULL,
    request_id       UUID,
    stream           BOOLEAN NOT NULL DEFAULT false,
    prompt_tokens    INTEGER,
    completion_tokens INTEGER,
    cached_tokens    INTEGER NOT NULL DEFAULT 0,        -- KPI de economia (cache de contexto)
    total_tokens     INTEGER,
    latency_ms       INTEGER,
    status_code      INTEGER,
    error_class      TEXT,                               -- 'rate_limit','auth','timeout','5xx','quota'
    client_ip        INET,
    client_ua        TEXT,
    cost_usd         NUMERIC(10,6),                      -- capturado no momento do request
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

COMMENT ON TABLE  usage_logs IS 'Logs de uso por requisição. PARTICIONADA mensal. FK lógica (sem FK física). Retention 90 dias.';
COMMENT ON COLUMN usage_logs.cached_tokens IS 'Tokens atendidos por cache de contexto no provedor (economia real). KPI do dashboard.';
COMMENT ON COLUMN usage_logs.cost_usd IS 'Custo calculado no momento do request (nullable no MVP, popula quando cost tracking chegar).';

-- Índices globais propagam para todas as partições. organization_id à esquerda.
CREATE INDEX idx_usage_logs_user_time    ON usage_logs (organization_id, user_id,    created_at DESC);
CREATE INDEX idx_usage_logs_provider_time ON usage_logs (organization_id, provider_id, created_at DESC);
CREATE INDEX idx_usage_logs_time         ON usage_logs (organization_id, created_at DESC);

-- -----------------------------------------------------------------------------
-- 8.1 Partições de usage_logs.
--     Pré-criadas: mês corrente (2026-06) + 3 meses à frente (07, 08, 09).
--     ATENÇÃO OPERACIONAL: NUNCA deixar faltar partição futura — INSERT falha.
--     Um CronJob ou job da aplicação cria as próximas partições periodicamente
--     (alternativa a pg_partman). Alerta se partição do próximo mês ausente.
-- -----------------------------------------------------------------------------
CREATE TABLE usage_logs_2026_06 PARTITION OF usage_logs
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');
CREATE TABLE usage_logs_2026_07 PARTITION OF usage_logs
    FOR VALUES FROM ('2026-07-01 00:00:00+00') TO ('2026-08-01 00:00:00+00');
CREATE TABLE usage_logs_2026_08 PARTITION OF usage_logs
    FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00');
CREATE TABLE usage_logs_2026_09 PARTITION OF usage_logs
    FOR VALUES FROM ('2026-09-01 00:00:00+00') TO ('2026-10-01 00:00:00+00');

-- Tabela default para capturar INSERTs fora de qualquer partição (sinal de alerta).
-- Se um INSERT cai aqui, é porque faltou criar a partição do mês — alerta crítico.
CREATE TABLE usage_logs_default PARTITION OF usage_logs DEFAULT;

-- -----------------------------------------------------------------------------
-- 9. usage_rollups — agregação HORÁRIA para dashboard (spec §3.4.1 item 9).
--    TABELA FÍSICA (não materialized view): permite upsert incremental
--    (INSERT ... ON CONFLICT DO UPDATE) sem REFRESH bloqueante.
--    PK composta permite agrupar por qualquer dimensão via SUM + GROUP BY.
--
--    NOTA: rollup DIÁRIO não é criado no MVP. Para janelas > 7d, o dashboard
--    deriva via date_trunc('day', bucket_hour) — ainda rápido com a PK.
--    Se profiling justificar, adicionar usage_rollups_daily em migration futura.
-- -----------------------------------------------------------------------------
CREATE TABLE usage_rollups (
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    organization_id   BIGINT NOT NULL DEFAULT 1,
    user_id           BIGINT NOT NULL,
    provider_id       BIGINT NOT NULL,
    model             TEXT NOT NULL,
    bucket_hour       TIMESTAMPTZ NOT NULL,
    request_count     BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cached_tokens     BIGINT NOT NULL DEFAULT 0,
    total_tokens      BIGINT NOT NULL DEFAULT 0,
    error_count       BIGINT NOT NULL DEFAULT 0,
    total_latency_ms  BIGINT NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id, provider_id, model, bucket_hour)
);

-- Índices para consultas do dashboard por dimensões alternativas.
CREATE INDEX idx_rollups_provider_hour ON usage_rollups (organization_id, provider_id, bucket_hour DESC);
CREATE INDEX idx_rollups_model_hour    ON usage_rollups (organization_id, model, bucket_hour DESC);
CREATE INDEX idx_rollups_hour          ON usage_rollups (organization_id, bucket_hour DESC);

COMMENT ON TABLE usage_rollups IS 'Agregação horária para dashboard. Upsert incremental (ON CONFLICT DO UPDATE). cached_tokens alimenta KPI de economia.';

-- -----------------------------------------------------------------------------
-- 10. audit_log — APPEND-ONLY (spec §3.4.1 item 10, Security §10.5).
--     Sem FK física (pode referenciar entidades deletadas no futuro).
--     Append-only garantido via GRANTs (role aipp_app só tem INSERT, nunca
--     UPDATE/DELETE). Os GRANTs não são emitidos aqui (operados via IaC/Helm)
--     porque CREATE ROLE exige privilégios de superuser indisponíveis em alguns
--     managed DBs. Ver comentário no final do arquivo.
-- -----------------------------------------------------------------------------
CREATE TABLE audit_log (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT 1,
    actor_user_id   BIGINT,                               -- NULL p/ ações do sistema
    action          TEXT NOT NULL,                        -- 'provider_key.disable', 'user.update'...
    entity_type     TEXT NOT NULL,                        -- 'user','provider_key','api_token'...
    entity_id       BIGINT,
    metadata        JSONB,
    ip              INET,
    user_agent      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_actor_time  ON audit_log (organization_id, actor_user_id, created_at DESC);
CREATE INDEX idx_audit_entity_time ON audit_log (organization_id, entity_type, entity_id, created_at DESC);

COMMENT ON TABLE audit_log IS 'Log de auditoria APPEND-ONLY. GRANT apenas INSERT para aipp_app (sem UPDATE/DELETE). Ver doc bloqueio abaixo.';

-- -----------------------------------------------------------------------------
-- 11. usage_logs_dead_letter — captura INSERTs de log que falham (spec §3.4.2).
--     Evita perda silenciosa quando o batch insert falha (partição ausente,
--     DB indisponível, erro de schema). Reprocessável via job de reconciliação.
--     NOTA: /tmp/usage-dlq/ (arquivo) protege de falha transitória; esta tabela
--     protege de falhas persistentes e é durável (WAL). Complementares.
-- -----------------------------------------------------------------------------
CREATE TABLE usage_logs_dead_letter (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payload     JSONB NOT NULL,                           -- registro original serializado
    error       TEXT NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0,
    resolved_at TIMESTAMPTZ,                              -- NULL = pendente de reprocessamento
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dead_letter_pending ON usage_logs_dead_letter (created_at) WHERE resolved_at IS NULL;

COMMENT ON TABLE usage_logs_dead_letter IS 'Fila de retrabalho para INSERTs de usage_logs que falharam. Resolve silenciosamente perdas (spec §3.4.2).';

-- =============================================================================
-- Observação sobre roles PostgreSQL (least-privilege, spec §3.3.7, Security §6.4):
--
-- A política de dois roles (aipp_app R/W sem DROP/TRUNCATE; aipp_migrations
-- com DDL rodando apenas em deploy) NÃO é aplicada aqui via CREATE ROLE porque:
--   1. CREATE ROLE exige privilégios indisponíveis em alguns managed DBs.
--   2. Roles são responsabilidade de IaC (Terraform) ou Helm Job, não do schema.
--
-- Recomendação documentada para o Helm Chart:
--   CREATE ROLE aipp_app WITH LOGIN PASSWORD '<from-secret>';
--   GRANT SELECT, INSERT, UPDATE         ON ALL TABLES IN SCHEMA public TO aipp_app;
--   GRANT INSERT                         ON audit_log TO aipp_app;  -- append-only
--   GRANT USAGE, SELECT                  ON ALL SEQUENCES IN SCHEMA public TO aipp_app;
--   REVOKE UPDATE, DELETE                ON audit_log FROM aipp_app; -- força append-only
-- =============================================================================
