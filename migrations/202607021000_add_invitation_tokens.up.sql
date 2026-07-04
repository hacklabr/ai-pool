-- =============================================================================
-- Migration 202607021000: Tabela de tokens de convite de usuários.
--
-- O token de convite é um segredo de uso único entregue ao admin para
-- compartilhar com um novo usuário. Apenas o hash SHA-256 do token é
-- persistido; o valor raw só existe no momento da geração e na URL do
-- comando de setup.
-- =============================================================================

CREATE TABLE invitation_tokens (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT 1 REFERENCES organizations(id),
    token_hash      TEXT NOT NULL,
    token_prefix    TEXT NOT NULL,
    username        TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'user'
                        CHECK (role IN ('admin', 'user')),
    status          TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'used', 'revoked')),
    created_by      BIGINT REFERENCES users(id),
    used_by         BIGINT REFERENCES users(id),
    used_at         TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unicidade parcial: um mesmo hash não pode estar ativo em dois convites.
CREATE UNIQUE INDEX idx_invitation_tokens_active_hash
    ON invitation_tokens (token_hash)
    WHERE status = 'pending';

-- Listagem admin por organização e status.
CREATE INDEX idx_invitation_tokens_org_status
    ON invitation_tokens (organization_id, status, created_at DESC);

COMMENT ON TABLE invitation_tokens IS 'Tokens de convite de uso único para onboarding de novos usuários.';
COMMENT ON COLUMN invitation_tokens.token_hash IS 'SHA-256 do token raw. O token completo NUNCA é persistido.';
COMMENT ON COLUMN invitation_tokens.token_prefix IS 'Prefixo do token (8 chars) para auditoria e listagem.';
