# Analysis: Database Administrator — Turn 2

**Agent ID:** software-development-database-administrator
**Timestamp:** 2026-06-25T18:55:32.305Z

# Delta DBA (Turno 2) — Alinhamento com os pares

Li integralmente as 5 análises (Software Architect, Backend Architect, Security Architect, DevOps, Frontend Architect). Há **consenso forte** na maior parte (PostgreSQL, golang-migrate, hexagonal, particionamento de logs, abstração de store, criptografia envelope). Identifiquei **6 tensões reais** e **2 concessões** que faço integralmente. Debate direto abaixo.

---

## T1. `organization_id` NOT NULL (minha posição) vs nullable (Software Architect Opção B, Backend, Security D11, DevOps 10.3)

**Situação**: estou isolado. Todos os pares propuseram `tenant_id NULLABLE`. Eu propus `NOT NULL DEFAULT 1` com seed em `organizations`.

**Argumento dos pares (nullable)**: "default NULL = sem tenant = single-tenant puro; migração trivial depois porque a coluna já existe".

**Minha defesa de NOT NULL**: o problema de nullable é que ele **permite** inconsistência — inserts esquecem de setar, queries esquecem de filtrar, e quando o multi-tenant chega, descobre-se que metade dos registros tem NULL e metade não. NOT NULL com default força consistência desde a primeira linha.

**Porém, concedo o ponto central**: nullable vs NOT NULL **não é a decisão que importa**. O que importa é:
1. **Todos os queries já filtrarem por `organization_id` desde o MVP** (mesmo que sempre = 1).
2. **Índices composite começam por `organization_id`** (ex.: `(organization_id, user_id, created_at)`).
3. **Todas as inserts recebem default org**.

Se estas 3 condições valerem, nullable e NOT NULL são funcionalmente equivalentes para a migração futura.

**Compromisso proposto (converjo, com requisitos)**:
```sql
organization_id BIGINT NULL DEFAULT 1 REFERENCES organizations(id)
```
- Nullable (atende pares) + default 1 (atende meu princípio de consistência).
- **Condição inegociável**: índices composite sempre com `organization_id` à esquerda; queries sempre filtram `WHERE organization_id = $1`; regra de lint no repositório que rejeita query em tabela multitenant sem filtro de `organization_id`.
- Seed `organizations(id=1, slug='default')` no migration inicial.

Isto absorve 95% do benefício do NOT NULL sem a rigidez. **Aceito**.

> Pergunto ao Backend Architect: concorda em formalizar a regra "toda query em tabela multitenant filtra por organization_id" como check de code review / linter?

---

## T2. Hash de token: SHA-256 puro (meu) vs HMAC-SHA256 com pepper + key_id separado (Security §4.2, D3) — **CONCEDO INTEGRALMENTE**

**Security Architect venceu este debate.** O argumento é tecnicamente superior:

- **HMAC com pepper** protege contra rainbow tables **mesmo se o dump do DB vazar**. SHA-256 puro, embora a entropia do token (~285 bits no formato dele) dispense brute-force, fica exposto a um dump — attacker com o dump pode correlacionar tokens offline se conhecer o algoritmo.
- **Separação `key_id` (público, 8 chars, indexado) + `secret` (HMAC hasheado)** mantém lookup O(1) pelo `key_id` **e** permite pepper no secret. É estritamente melhor que meu "SHA-256 direto por todo o token".

**Ajusto meu schema (turno 1 estava errado)**:
```sql
api_tokens(
  ...
  key_id        CHAR(8)      NOT NULL UNIQUE,   -- parte pública, lookup O(1)
  secret_hash   BYTEA        NOT NULL,           -- HMAC-SHA256(secret, pepper)
  ...
)
CREATE UNIQUE INDEX ON api_tokens(key_id) WHERE revoked_at IS NULL;
```
- Pepper = segredo no KEK/External Secrets (nunca no DB, nunca no backup do DB).
- Comparação via `hmac.Equal` (constant-time) —Security §4.3.
- Lookup: parse do token → extrai `key_id` → SELECT pelo índice UNIQUE → `hmac.Equal` no secret.
- Adicional do Security: prefixo `sk-aipp-` para GitHub secret scanning (D13) — **excelente**, adoto.

**Retroativo**: meu turno 1 disse "SHA-256 determinístico basta porque o token tem alta entropia". Estava certo sobre brute-force, mas **errado sobre defesa em profundidade** (pepper protege contra dump). Corrigido.

---

## T3. golang-migrate + integração com a camada de persistência (Backend §10, Software §4.4) — **CONSENSO, CONFIRMO**

Não há tensão. Confirmo **golang-migrate** com arquivos `.up.sql`/`.down.sql` SQL-puros (não código Go). Alinhamento com:
- **Backend** (`/migrations/`, repositórios `repository/` com pgx).
- **Software** (golang-migrate, hexagonal, domínio puro).
- **DevOps** (Helm Hook Job com `restartPolicy: OnFailure`, advisory lock no PG).

**Acrescento um detalhe operacional**: a aplicação do migration via **Helm pre-install/pre-upgrade Hook Job** (DevOps §5.3) é preferível a init-container no Deployment quando houver >1 réplica, porque evita N pods competindo pela migration. Para 1 réplica, init-container é OK. O advisory lock do golang-migrate (`pg_advisory_lock`) protege ambos os casos.

**Sobre ORM**: não prescrevi no turno 1, mas endosso a recomendação conjunta Software/Backend: **pgx + sqlc**. sqlc gera código type-safe a partir de SQL puro versionado — casa com meu princípio "pensar em SQL, não em ORM". Beego ORM e GORM AutoMigrate são **proibidos em produção** (DevOps §5.3, concordo). O `domain/` do Software Architect define interfaces; o `repository/` do Backend as implementa com pgx/sqlc; migrations são SQL — camadas limpas, zero acoplamento entre domínio e SQL.

---

## T4. Volume de `usage_logs` + durabilidade do canal em memória (Backend §9.3)

**Alinhamento forte**, mas há um ponto de atrito sobre **perda aceitável**.

Backend Architect propôs: canal `chan UsageRecord` cap 1000, non-blocking send (`select` + `default` → descarta se cheio), worker batch insert (50 records ou 500ms). **Aceito integralmente** — é a estratégia correta para 30 usuários.

**Tensão**: o Backend aceita "descartar se canal cheio" e "logar localmente (stderr)". Eu disse "descartar com backpressure, registrar métrica de drops". Estamos alinhados **exceto em uma sutileza**: o Backend não mencionou que o **particionamento mensal + pré-criação de partições** é pré-requisito para que o batch insert não falhe catastróficamente.

**Risco concreto que adiciono**: se a partição do mês corrente não existir (falta de automação pg_partman/CronJob), **TODOS** os batch inserts falham → worker entope → canal enche → todos os logs subsequentes são descartados silenciosamente. Em 1 hora, perde-se 100% dos logs de uso sem alerta óbvio.

**Mitigação que peço no spec**:
1. **Pre-criação de partições**: sempre criar os próximos 2 meses + alerta se partição futura ausente (verifiquei no turno 1, §3.2).
2. **Métrica Prometheus** `usage_log_insert_failures_total` (mesmo com `/metrics` desativado no MVP, a instrumentação existe — alinhado com DevOps §7.3).
3. **Dead-letter**: se batch insert falha N vezes, gravar o lote em arquivo local (`/tmp/usage-dlq/`) para reprocessamento posterior, não apenas descartar. Custo baixo, salva auditoria em incidente.

**Sobre nomenclatura**: Backend chamou de `usage_records`, eu de `usage_logs`. **Converjo para `usage_logs`** — semanticamente "log" (append-only, auditável) é mais preciso que "record". Peço padronização no spec.

**Confirmação**: meu particionamento mensal + retention 90 dias (DROP de partição) + rollups horário/diário é **compatível** com o batch insert do Backend. O worker deve usar `INSERT INTO usage_logs (...) VALUES (...),(...),(...)` multi-row com prepared statement; em caso de failover de partição (mês virando), o insert cai naturalmente na partição nova. Zero atrito técnico.

---

## T5. Rollups (tabelas) vs materialized views — **CONFIRMO, RESOLVE O DASHBOARD**

Frontend Architect (§3) precisa de:
```
GET /api/v1/web/dashboard/usage?from=&to=&granularity=hour|day&group_by=user|provider|model
→ { series: [...], buckets: [...] }
```

**Minha rollup resolve isto diretamente**:
- `usage_rollups_hourly(organization_id, user_id, provider_id, model_name, bucket_hour, request_count, prompt_tokens, completion_tokens, total_tokens, error_count, avg_latency_ms)` com PK composta.
- Para `group_by=user`: `SELECT user_id, SUM(total_tokens) FROM usage_rollups_hourly WHERE bucket_hour BETWEEN $1 AND $2 AND organization_id=$3 GROUP BY user_id`.
- Para `group_by=provider`: mesma estrutura, troca a dimensão do GROUP BY.
- `granularity=hour` consulta `usage_rollups_hourly`; `granularity=day` consulta `usage_rollups_daily` (ou `date_trunc('day', bucket_hour)` na hourly se a daily ainda não estiver populada).

**Por que tabelas e não `MATERIALIZED VIEW`** (reforço minha posição do turno 1):
- `REFRESH MATERIALIZED VIEW` é **bloqueante** (a menos que CONCURRENTLY, que exige UNIQUE index e ainda duplica storage durante o refresh).
- Tabelas permitem **upsert incremental** (`INSERT ... ON CONFLICT (...) DO UPDATE`) — só agrega a última hora fechada, sem reprocessar histórico.
- Para o endpoint do Frontend, query na rollup é sub-milissegundo; cache 60s no handler (Frontend §3) é redundante mas inofensivo.
- O Frontend pede **no máximo 365 pontos** (Frontend §3) — a rollup diária cobre 1 ano de pontos com folga.

**Sem tensão real**. Confirmo que rollups tabelas + endpoint JSON do Frontend casam perfeitamente. O Backend Architect (§13) mencionou "VIEW materializada ou rollup job" — alinhamos para **rollup job** (pg_cron ou aplicação), tabelas físicas.

---

## T6. ~12 entidades — schema inflado? **CONCEDO PARCIALMENTE, REDUZO PARA ~10**

Software Architect (§5) e Backend (§11) propuseram schemas mais enxutos. Identifiquei 3 entidades que **posso deferir sem perder extensibilidade**:

### T6.1. `model_aliases` como tabela → **converjo para JSONB em `models`** (Software ADR-003)
Software Architect propôs `model.aliases jsonb`. Para o MVP (aliasing simples `gpt-4 → modelo_interno`), JSONB basta. Tabela separada só vale se houver regras complexas (target por provider, prioridade, is_active por alias) — caso que só aparece com "model auto routing" no roadmap.
- **Ação**: `models.aliases JSONB NULL` no MVP. Deferir `model_aliases` tabela.

### T6.2. `model_pricing` como tabela → **converjo para colunas nullable em `models`** (Software ADR-003)
Software Architect: `model.cost_in_per_1k, cost_out_per_1k nullable`. Para o MVP, preço atual (uma linha por modelo) basta. Tabela de histórico de preços (`effective_from`/`effective_until`) só vale quando cost tracking real exigir auditoria financeira.
- **Ação**: `models.cost_in_per_1k NUMERIC(12,6) NULL`, `cost_out_per_1k NUMERIC(12,6) NULL`, `models.cached_in_per_1k NUMERIC(12,6) NULL` (para cache de contexto — DeepSeek/Gemini têm preço diferenciado). Deferir `model_pricing` tabela.
- Coluna `usage_logs.cost_usd NUMERIC(10,6) NULL` **permanece** (barata, captura custo no momento do request mesmo que preço mude depois).

### T6.3. `teams`, `team_members`, `quotas` → **defiro para roadmap** (Security, Backend, Software)
Eu propus criar vazias desde já. Mas:
- `organization_id` já pavimenta o multi-tenant (a relação mais cara de migrar).
- Teams sem enforcement não trazem valor; criar vazias é YAGNI.
- Quotas: `usage_logs` + rollups já fornecem o dado bruto; a tabela de limites só precisa existir quando enforcement chegar.
- **Ação**: deferir as 3 tabelas. Documentar no ROADMAP que `organization_id` está pronto e teams/quotas são próxima onda.

**Schema reduzido do MVP (10 entidades)**:
1. `organizations` (seed id=1)
2. `roles` (seed admin, user)
3. `users`
4. `api_tokens` (com `key_id` + `secret_hash` HMAC — correção T2)
5. `providers`
6. `models` (com `aliases JSONB`, `cost_*` nullable — correção T6.1/T6.2)
7. `provider_keys` (chaves encriptadas, estado, scheduler fields)
8. `rate_limits` (config)
9. `usage_logs` (particionada mensal)
10. `usage_rollups_hourly` + `usage_rollups_daily` (conto como 1 bloco "rollups")
11. `audit_log`

**~11 entidades** — alinhado com Software/Backend. Menos inflado, mesma extensibilidade.

---

## T7. Fonte da verdade do estado de chave: DB write-through (meu turno 1) vs in-memory + sync periódico (Software §7.1, Backend §7.4) — **COMPROMISSO HÍBRIDO**

**Tensão real**. Meu turno 1 enfatizou "UPDATE condicional atômico no DB a cada transição de estado". Software/Backend enfatizaram "mutex in-memory no hot path + sync periódico para o DB". 

**Análise**:
- **Contadores de erro** (`error_count`, `consecutive_errors`, `last_error_at`): no hot path, há 1-2 atualizações por request em caso de erro. Fazer UPDATE no DB a cada erro = round-trip desnecessário no caminho quente. **In-memory + persistência periódica (a cada 5s ou em batch)** é correto.
- **Transição de estado** (`enabled → auto_disabled`, `auto_reactivate_at`): esta é a transição que **afeta o scheduler, outras réplicas e o dashboard**. Deve ser **write-through imediato** ao DB (UPDATE atômico condicional `WHERE id=$1 AND status='enabled' RETURNING ...`). Não dá para esperar sync periódico de 5s — outra réplica poderia selecionar a chave problemática nesse intervalo.
- **`last_used_at`** (LRU): puramente in-memory para seleção; persistência periódica best-effort (debounce 60s) só para reconstrução no boot. (Concordo com Backend §4.5: LRU é heurística, não persistir como fonte de verdade.)

**Compromisso articulado**:
- **Hot path (seleção LRU, contadores de erro)**: in-memory com mutex, sync periódico ao DB (configurável, default 5s).
- **Transição de estado de chave**: **write-through imediato** com UPDATE atômico condicional + `RETURNING`. Idempotente (a condição `status='enabled'` impede dupla transição).
- **Scheduler de reativação**: `SELECT ... FOR UPDATE SKIP LOCKED` no DB (minha posição turno 1) — não no in-memory, porque precisa sobreviver a restart e ser consistente entre réplicas.

Pergunto ao Backend Architect: este híbrido (in-memory para contadores, write-through para transições) é compatível com seu `Key State Manager` (§7)? Acredito que sim.

---

## T8. CloudNativePG (meu turno 1) vs Managed DB (DevOps §5.1) — **CONCEDO AO DEVOPS**

DevOps argumentou corretamente: para equipe pequena + 30 usuários, o custo operacional de self-host PG (backups, replicas, upgrades, vacuum, WAL) é desproporcional. Managed DB (CloudSQL/RDS/Azure) oferece PITR, HA multi-AZ, patches, failover automático.

**Concedo**: Managed DB como **default** para produção. CloudNativePG fica como **exceção** para on-prem/soberania de dados/custo cloud proibitivo. A decisão é do DevOps/SRE, não do DBA.

**Mantenho apenas uma recomendação**: seja managed ou CNPG, **PITR via WAL archiving** é inegociável (recupera de "apagão acidental" até segundos antes). E **DR drill mensal** (restaurar em ambiente efêmero, testar). Backup não testado não é backup.

---

## T9. Backup encryption (Security §3.6) — **REFORÇO, ALINHAMENTO TOTAL**

Security Architect disse: "Backup do PostgreSQL deve ser criptografado; KEK nunca entra no backup; restaurar backup sem KEK = dado inutilizável (propriedade desejada)". **Concordo integralmente** e adiciono:
- Managed DB: snapshots criptografados por default (CMEK se possível — Customer Managed Encryption Key).
- Self-host CNPG: WAL archive para S3 com SSE-KMS; `pg_dump` pipe para `gpg --encrypt`.
- KEK em External Secrets/Vault (DevOps §3.6) — **nunca** no mesmo bucket/repo do backup do DB. Separação física.

---

## T10. FK em `usage_logs` particionada — **RISCO ISOLADO, NENHUM PEER COMENTOU**

Nenhum dos 5 pares mencionou que **tabela particionada não admite FK física** em PostgreSQL (até v18; v19 trouxe melhorias parciais). Minha posição do turno 1: FK **lógica** (sem CONSTRAINT), integridade por aplicação.

**Por que isto importa**: se um usuário é soft-deletado ou uma `provider_key` é removida, os `usage_logs` antigos podem ficar com `user_id`/`provider_key_id` órfãos. Para auditoria isto é **aceitável** (o log registra o que aconteceu naquele momento), mas o dashboard pode quebrar se fizer JOIN cego.

**Mitigação que peço no spec**:
1. `users` e `provider_keys` são **soft-delete** (`deleted_at`), nunca DELETE físico. Logs mantêm referência válida.
2. JOINs do dashboard usam `LEFT JOIN` (registros órfãos aparecem como "unknown user/key" em vez de sumirem).
3. Job de validação periódica (weekly) verifica referências órfãs e reporta.

---

## T11. Concorrência em `last_used_at` de tokens/chaves — **ALINHAMENTO COM DEBOUNCE**

Nenhum peer rebateu meu debounce de 60s para `last_used_at` (turno 1 §7.3). Backend (§9.1) lista `last_used_at` como campo do log por request, mas não detalha frequência de UPDATE na `api_tokens`/`provider_keys`. 

**Confirmo a recomendação**: atualizar `last_used_at` a cada request = write amplification (mesma linha atualizada N vezes/segundo sob carga). Solução:
- In-memory "dirty set" por entidade (token_id → último flush timestamp).
- Flush em batch a cada 60s (ou N updates) via `UPDATE api_tokens SET last_used_at=$1 WHERE id IN (...)`.
- Aceita-se imprecisão ≤60s em troca de MUITO menos writes.

Não há tensão, apenas **omissão** nos outros analyses. Mantenho a recomendação.

---

## Resumo do delta (para o Manager)

**Concessões que faço** (mudam meu turno 1):
1. **T2 — Token hashing**: converjo para HMAC-SHA256 com pepper + `key_id` separado (Security venceu tecnicamente).
2. **T6 — Schema**: reduzo de ~13 para ~11 entidades. `model_aliases` vira JSONB; `model_pricing` vira colunas nullable; `teams`/`quotas` deferidas para roadmap.
3. **T8 — Backup infra**: aceito Managed DB como default (DevOps); CNPG como exceção.
4. **T1 — organization_id**: converjo para nullable `DEFAULT 1` **com** requisitos (índices composite, queries sempre filtram, regra de lint).

**Posições que mantenho** (reforçadas):
5. **T4 — Particionamento + pré-criação de partições + dead-letter** para durabilidade dos logs.
6. **T5 — Rollups como tabelas** (não materialized views) — resolvem o endpoint do Frontend.
7. **T7 — Híbrido estado de chave**: in-memory para contadores, **write-through** para transições de estado, DB para scheduler.
8. **T10 — FK lógica em `usage_logs`** + soft-delete em `users`/`provider_keys` + LEFT JOIN no dashboard.
9. **T11 — Debounce 60s** para `last_used_at`.

**Consenso confirmado** (sem tensão): PostgreSQL, golang-migrate, pgx + sqlc, criptografia envelope (DEK/KEK), hexagonal, particionamento mensal, retention por DROP, store abstraction (memory + Redis-ready), CloudNativePG como alternativa on-prem.

**Perguntas direcionadas**:
- **Backend**: T1 (regra de lint para filtro de `organization_id`)? T7 (híbrido estado de chave compatível com seu Key State Manager)?
- **Security**: confirma pepper no KEK/External Secrets, nunca no DB?
