# ADR-0010 — Envelope encryption AES-256-GCM para chaves de provedores, com KEK externa

**Status:** aceito (com ressalva operacional)

**Contexto:** `provider_keys.encrypted_value` é o ativo mais sensível do sistema. Cifrar com chave derivada do próprio DB não protege contra dump; KEK precisa viver fora do banco. A entrega da KEK ao pod deve evitar exposição em env vars (`/proc/<pid>/environ`). O consenso original previa External Secrets Operator (ESO) como mecanismo de entrega.

**Decisão:** Envelope encryption com AES-256-GCM: KEK de 32 bytes carregada de arquivo montado readOnly (`KEK_PATH`); formato persistido `nonce(12) || ciphertext || tag(16)`; coluna `encryption_key_id` versiona a KEK, permitindo rotação gradual (múltiplas KEKs registradas; `SetActiveKeyID` para novas escritas). DEV_MODE sem KEK usa `IdentityDecryptor` (plaintext) com WARN loud no log. KEK nunca é env var, nunca logada, nunca no DB.

**Consequências:** Positivas: dump do DB não revela chaves; integridade via auth tag do GCM (tampering → erro); rotação sem janela de indisponibilidade. Negativas: perda da KEK = perda irrecuperável das chaves (backup da KEK é responsabilidade operacional). **Ressalva/divergência:** embora o chart suporte ESO (`externalSecret.enabled`), a produção atual usa Secret K8s criado manualmente via `kubectl` — a decisão "KEK via ESO" está implementada como capacidade, não como prática vigente; alinhar doc ou operação (doc-bug aberto no setup).

**Evidências:** `internal/adapters/crypto/envelope.go`; `cmd/aipp/main.go` (buildDecryptor/buildEncryptor); `deploy/helm/ai-pool/templates/externalsecret.yaml`; `deploy/helm/ai-pool/values-prod.yaml` (divergência); `internal/adapters/store/identity_decryptor.go`.

**Origem:** DECISIONS.md A10 + A13 (fundidas: A13 é o mecanismo de entrega da KEK da A10).
