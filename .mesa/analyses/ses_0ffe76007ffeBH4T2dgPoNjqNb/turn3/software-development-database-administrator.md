# Posição Final de Consenso — DBA (Turno 3, 5º a falar)

Posiciono-me em relação aos 4 pares que já votaram (Software, Backend, DevOps, Security — todos Opção A). **Meu voto: AGREE com o consenso emergente.** Fecho abaixo todas as decisões de dados sob minha responsabilidade, sem ressalvas bloqueantes.

---

## 1. Réplicas: Opção A confirmada (1 réplica no MVP)

**Alinho com os 4 pares.** Single-réplica no MVP, com `replicas: 2` apenas quando graceful shutdown + Redis estiverem prontos (MVP+1, conforme DevOps §9.3).

**Implicações concretas para a camada de dados** (que são a minha alçada):

1. **Sem locks distribuídos necessários** — não preciso de Redis advisory lock, nem de eleição de líder para o scheduler. O `SELECT ... FOR UPDATE SKIP LOCKED` que propus no turno 1 para o scheduler de reativação permanece **correto e recomendado** mesmo com 1 pod, porque: (a) é à prova de futuro (prepara para multi-pod), (b) custo zero em single-pod, (c) documenta a intenção de fila de trabalho no SQL. **Mantenho a recomendação.**
2. **Consistência simplificada**: single-writer no Postgres single-node = strong consistency trivial. `READ COMMITTED` (default) basta. **Não há necessidade de `SERIALIZABLE`** nem de controle de concorrência externo.
3. **Estado de chave em memória é fonte de verdade no hot path** (alinhado a Software §7.1 e Backend §7.4), com DB como persistência de recuperação. O híbrido que articulei na T7 (in-memory para contadores, **write-through imediato** para transições de estado) é **especialmente correto em single-réplica** — não há segunda réplica que precise ser notificada, mas o write-through garante durabilidade em restart.
4. **Connection pooling**: `pgxpool` com `max_conns=25` (DevOps §5.2) é farto para 1 pod × 30 usuários. PgBouncer **não é necessário no MVP** — entra no roadmap quando réplicas crescerem.
5. **Cache positivo de token** (Security D4): single-pod dispensa invalidação cross-réplica. Posso até adicionar um cache L1 in-memory TTL 30s sem violar "revogação instantânea" — basta `PurgeUser()` local no disable. Mas aceito a posição mais conservadora do Security (DB hit por request) pela simplicidade. **Decisão deixa ao Backend; do ponto de vista de DB, query indexada em `key_id` é sub-milissegundo, sem impacto perceptível.**

---

## 2. Tabelas vazias: **DEFIRO integralmente** teams / quotas / time_window_limits

**Mudo minha posição do turno 1** (queria criar vazias desde já). Aceito o consenso 4-1 (Software, Backend, DevOps deferem; Security implícito). A razão pela qual **deferir é seguro do ponto de vista de dados**:

- **`organization_id` já pavimenta a relação mais cara de migrar** — aquela que toca `usage_logs` particionada (backfill em tabela particionada = pesado e arriscado). Teams/quotas/time_window_limits são **tabelas novas isoladas** — adicioná-las depois é migration trivial (`CREATE TABLE`), sem tocar tabelas existentes.
- **Dados brutos para quotas já existem**: `usage_logs.total_tokens` + `usage_rollups_*` permitem calcular consumo agregado por `(organization_id, user_id, período)` desde já. A tabela `quotas` (limites) só é necessária quando o **enforcement** chegar — dado sem enforcement é inerte.
- **time_window_limits** não exige schema de forma alguma (limite de horário é config, vive em JSONB numa futura tabela `policies`). Nada a adicionar agora.

**Conclusão**: deferir é a decisão correta. YAGNI vence aqui. Documento no ROADMAP que `organization_id` está pronto e teams/quotas/time_window_limits são a próxima onda, com migration trivial.

---

## 3. Schema final consolidado do MVP (~11 entidades)

Confirmação canônica (alinhado com T6 do meu delta, endossado pelos pares):

| # | Tabela | Notas |
|---|---|---|
| 1 | `organizations` | Seed `id=1, slug='default'`. |
| 2 | `roles` | Seed `admin`, `user`. FK em `users`. |
| 3 | `users` | `organization_id`, `role_id`, `password_hash` (argon2id), soft-delete `deleted_at`. |
| 4 | `api_tokens` | `key_id CHAR(8) UNIQUE` (lookup O(1)) + `secret_hash BYTEA` (HMAC-SHA256+pepper). Soft-delete `revoked_at`. |
| 5 | `providers` | `organization_id`, `base_url`, `api_spec`, `user_agent_override`. |
| 6 | `models` | `aliases JSONB`, `context_window`, `cost_in_per_1k`/`cost_out_per_1k`/`cached_in_per_1k` nullable, `modalities JSONB`. |
| 7 | `provider_keys` | `encrypted_value BYTEA` (AES-256-GCM), `encryption_key_id`, `status` enum, `auto_reactivate_at`, contadores de erro, `user_agent_override`. |
| 8 | `rate_limits` | Config (scope_type, scope_id, rpm/rph/rpd). Runtime fica em store abstraído. |
| 9 | `usage_logs` | **Particionada mensal**. PK `(id, created_at)`. Sem FK física (lógica). |
| 10 | `usage_rollups_hourly` + `usage_rollups_daily` | Tabelas (não materialized views). PK composite. Inclui `cached_tokens` para KPI de economia de contexto. |
| 11 | `audit_log` | Append-only (GRANT sem UPDATE/DELETE). JSONB `before`/`after`. |

**Extensibilidade embutida sem custo**: `organization_id` em tudo, `cost_usd` nullable em `usage_logs`, `cached_tokens` em logs e rollups, `encryption_key_id` para rotação de KEK, `model.aliases/modalities/context_window` para roteamento futuro.

---

## 4. Decisões de DADOS do MVP — lista canônica fechada

Consolido tudo o que está sob minha responsabilidade (cada item com meu voto explícito):

| # | Decisão | Voto DBA |
|---|---|---|
| D1 | `organization_id BIGINT NULL DEFAULT 1 REFERENCES organizations(id)` em todas as tabelas multitenant + seed org default | ✅ AGREE (concedi na T1 com requisitos) |
| D2 | Índices composite sempre com `organization_id` à esquerda + queries sempre filtram + regra de lint | ✅ AGREE (condição inegociável que adicionei) |
| D3 | Nomenclatura: **`organization_id`** (não `tenant_id`) — mais descritivo, evita ambiguidade com teams/workspaces futuros | ⚠️ **Pequena divergência de naming** — peço padronização para `organization_id` no spec |
| D4 | Tokens: `key_id` (público, CHAR(8), UNIQUE indexado) + `secret_hash` (HMAC-SHA256 com pepper) + prefixo `sk-aipp-` | ✅ AGREE (concedi ao Security na T2) |
| D5 | Pepper do HMAC vive no KEK/External Secrets, **nunca** no DB nem no backup do DB | ✅ AGREE (Security) |
| D6 | Chaves de provedor: AES-256-GCM (envelope encryption: DEK por chave + KEK master externo) + `encryption_key_id` para rotação | ✅ AGREE |
| D7 | KEK em External Secrets Operator + backend cloud (DevOps §3.6); rotação de KEK no roadmap | ✅ AGREE |
| D8 | `usage_logs` **particionada declarativa mensal** (RANGE por `created_at`), PK `(id, created_at)` | ✅ AGREE (minha posição turno 1, confirmada) |
| D9 | Pré-criação automática de partições (pg_partman OU CronJob K8s) — sempre próximos 2 meses + alerta de partição ausente | ✅ AGREE (crítico operacional) |
| D10 | Retention: 90 dias de `usage_logs` granulares via `DROP` de partição; agregados em rollups por anos | ✅ AGREE |
| D11 | Rollups como **tabelas físicas** (`usage_rollups_hourly`/`usage_rollups_daily`) com upsert incremental, **não** materialized views | ✅ AGREE (minha posição turno 1) |
| D12 | Rollup job (pg_cron ou aplicação) agrega última hora fechada a cada hora | ✅ AGREE |
| D13 | FK **lógica** em `usage_logs` (sem CONSTRAINT física) + `users`/`provider_keys` soft-delete + LEFT JOIN no dashboard | ✅ AGREE (T10) |
| D14 | `golang-migrate` com `.up.sql`/`.down.sql` SQL-puros, via Helm pre-install/pre-upgrade Hook Job + advisory lock | ✅ AGREE (T3) |
| D15 | ORM: **pgx v5 + sqlc** (type-safe a partir de SQL); beego/orm e GORM AutoMigrate **proibidos em produção** | ✅ AGREE (T3) |
| D16 | Managed DB (CloudSQL/RDS/Azure) como **default** produção; CloudNativePG como exceção on-prem | ✅ AGREE (concedi ao DevOps na T8) |
| D17 | PITR via WAL archiving **inegociável** + backup encryption (CMEK ou pg_dump+gpg) + DR drill mensal | ✅ AGREE (T8/T9) |
| D18 | KEK **nunca** no backup do DB; separação física do backup do DB e do secret store | ✅ AGREE (T9) |
| D19 | Concorrência: `UPDATE ... WHERE status='enabled' RETURNING` (write-through atômico para transições de estado) — idempotente | ✅ AGREE (T7) |
| D20 | Scheduler de reativação: `SELECT ... FOR UPDATE SKIP LOCKED WHERE status='auto_disabled' AND auto_reactivate_at <= now()` | ✅ AGREE (T7) |
| D21 | Contadores de erro in-memory + sync periódico ao DB (5s default); `last_used_at` debounce 60s (dirty set + batch UPDATE) | ✅ AGREE (T7/T11) |
| D22 | `audit_log` append-only via GRANT sem UPDATE/DELETE para role `aipp_app`; dois roles PG (`aipp_app` R/W, `aipp_migrations` DDL) | ✅ AGREE (Security §6.4) |
| D23 | Tabelas `teams`/`team_members`/`quotas`/`time_window_limits`/`model_aliases` (tabela)/`model_pricing` (tabela) → **deferidas para roadmap** | ✅ AGREE (T6, convergi ao consenso) |
| D24 | Graceful shutdown no MVP (decisão dos pares) — sem impacto direto em schema, mas `synchronous_commit=on` mantido para durabilidade em drenagem | ✅ AGREE |

---

## 5. Compatibilidade com registro de uso assíncrono do Backend (canal + batch insert)

**Confirmação: totalmente compatível.** O desenho do Backend (§9.3) casa com meu particionamento. Detalho a análise de **durabilidade**, que é onde posso agregar valor:

### 5.1 Camadas de durabilidade

| Camada | Durabilidade em crash de pod | Mitigação |
|---|---|---|
| Request processado + resposta ao cliente | ✅ Garantida (já respondido) | — |
| Log no canal `chan UsageRecord` (cap 1000) | ❌ **Perde-se** em OOM/kill | Aceitar perda ≤ (batch_size × flush_interval) |
| Batch em trânsito para o worker | ❌ Perde-se | Mesmo acima |
| Batch INSERT em execução no Postgres | ⚠️ Depende do `COMMIT` | WAL garante após commit |
| Batch COMMITado no Postgres | ✅ Durável (WAL + replica) | — |

### 5.2 Janela máxima de perda aceitável

Com `batch_size=50` e `flush_interval=500ms` (Backend §9.3), a perda máxima em crash de pod é **~25-50 logs** (1-2 batches em trânsito). Para auditoria de **uso** (não transação financeira), isto é **aceitável** e documentado.

### 5.3 Garantias adicionais que peço no spec

1. **WAL é a garantia real de durabilidade** — após `COMMIT` do batch, o log sobrevive a crash do Postgres (e a replica, se houver). `synchronous_commit=on` (default) mantido. **Não** usar `synchronous_commit=off` para o worker de logs mesmo buscando latência — a perda de um batch confirmado ao cliente mas não durável quebra a auditoria.
2. **Dead-letter** (`/tmp/usage-dlq/`): protege contra **falha de INSERT** (partição ausente, DB indisponível, erro de schema). **Atenção**: `/tmp` em K8s é `emptyDir` — perde em restart do pod. Então dead-letter salva de **falha transitória de insert**, não de **crash catastrófico**. Para o segundo caso, a perda é aceita (ver 5.2). Documentar esta distinção.
3. **Métrica `usage_log_insert_failures_total`** (mesmo com `/metrics` desativado no MVP — instrumentação existe, DevOps §7.3) + alerta se taxa > 0 sustentada.
4. **Pré-criação de partições (D9)** é pré-requisito: sem partição do mês corrente, **TODOS** os batches falham → canal enche → silenciosamente descarta 100% dos logs. Esta é a falha mais provável e mais cara — alerta de "partição futura ausente" é prioridade operacional.

### 5.4 Nomenclatura

Backend usou `usage_records`; eu e o consenso convergimos para **`usage_logs`** (semântica de "log" append-only/auditável é mais precisa que "record"). Peço padronização no spec.

---

## 6. Pontos onde **discordo** do consenso (nenhum bloqueante)

Apenas **uma divergência menor de naming**, não de métrica:

- **D3**: pares usaram `tenant_id`; eu uso **`organization_id`**. Justificativa: `tenant` é termo ambíguo (pode significar team, workspace, organização, conta). Quando o roadmap trouxer **teams** dentro de organizações, `tenant_id` fica confuso (`tenant` = org ou team?). `organization_id` é inequívoco: organização é o tenant de topo, teams são sub-entidades. **Peço padronização para `organization_id`** no spec. Se o time preferir `tenant_id`, aceito — mas registro a recomendação.

**Nenhuma outra divergência técnica.** Todas as tensões da T2 foram resolvidas (T1 converged, T2 concedido, T6 reduzido, T8 concedido).

---

## 7. Perguntas remanescentes (não bloqueantes para consenso)

Direciono ao Backend Architect (perguntas que ficaram em aberto no delta T2):

1. **T7 (híbrido estado de chave)**: o `Key State Manager` (Backend §7) será desenhado para fazer write-through imediato ao DB em transições de estado (`enabled→auto_disabled`), mantendo contadores in-memory? É o desenho que garante durabilidade跨 restart sem custo no hot path.
2. **D2 (regra de lint `organization_id`)**: concorda em formalizar um check (ex.: sqlc query annotation, ou linter customizado) que rejeita query em tabela multitenant sem `WHERE organization_id = $1`?

Não uso `ask_peer` porque são perguntas de esclarecimento para o spec, não de bloqueio de consenso.

---

## 8. Voto final

**AGREE** com o consenso emergente:
- Opção A (1 réplica MVP + graceful shutdown) ✅
- Deferir teams/quotas/time_window_limits ✅
- Schema ~11 entidades consolidado ✅
- HMAC-SHA256+pepper, AES-256-GCM envelope, particionamento mensal, rollups tabelas, FK lógica, debounce 60s, golang-migrate, Managed DB default, write-through para transições de estado ✅
- Registro assíncrono do Backend compatível, com WAL como garantia de durabilidade e perda ≤50 logs aceitável em crash ✅

**Pronto para o Manager formalizar a especificação.** Todas as decisões de dados estão fechadas e articuladas.
