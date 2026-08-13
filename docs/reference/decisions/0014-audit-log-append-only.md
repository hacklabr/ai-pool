# ADR-0014 — audit_log append-only

**Status:** aceito (com ressalva de enforcement)

**Contexto:** Auditoria de ações administrativas (disable de chave, revogação de token, alteração de usuário) exige imutabilidade: um atacante com acesso à aplicação não deve poder apagar rastros.

**Decisão:** `audit_log` é append-only: sem FK física (pode referenciar entidades futuramente deletadas), a camada de aplicação expõe apenas `Append` e `List` (não há Update/Delete no repositório). O enforcement em nível de banco (role `aipp_app` com GRANT apenas INSERT + REVOKE UPDATE/DELETE) é recomendado, mas deliberadamente **não** aplicado na migration (CREATE ROLE exige privilégios ausentes em managed DBs) — fica como responsabilidade de IaC.

**Consequências:** Positivas: trilha de auditoria confiável por construção da aplicação; schema simples. Negativas: **ressalva** — enquanto os GRANTs não forem aplicados pela infra, o append-only é convenção de código, não garantia de banco; sem política de retenção definida (crescimento ilimitado — revisar junto com a política LGPD pendente de retenção de usage_logs). Cobertura atual é parcial (apenas convites gravam auditoria — dívida registrada no setup).

**Evidências:** `internal/adapters/postgres/audit_repo.go` (só Append/List); `migrations/202606252100_init_schema.up.sql` (audit_log + comentário de GRANTs).

**Origem:** DECISIONS.md A15 (consenso de especialistas 6-0).
