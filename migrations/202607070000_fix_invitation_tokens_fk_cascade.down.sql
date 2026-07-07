ALTER TABLE invitation_tokens
    DROP CONSTRAINT invitation_tokens_created_by_fkey,
    DROP CONSTRAINT invitation_tokens_used_by_fkey;

ALTER TABLE invitation_tokens
    ADD CONSTRAINT invitation_tokens_created_by_fkey
        FOREIGN KEY (created_by) REFERENCES users(id),
    ADD CONSTRAINT invitation_tokens_used_by_fkey
        FOREIGN KEY (used_by) REFERENCES users(id);
