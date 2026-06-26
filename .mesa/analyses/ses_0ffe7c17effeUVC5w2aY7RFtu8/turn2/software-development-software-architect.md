# Delta — Software Architect (Turno 2)

> Foco no que mudou, onde converjo, onde divirjo, e o que os pares erraram/omitiram. Não repito Turn 1.

---

## 1. Convergências que reforçam minha posição (validação cruzada)

Cinco pontos onde o consenso emergente **endurece** recomendações que fiz em Turn 1 — passo a defendê-las como **decididas**, não como propostas:

1. **`tenant_id`/`organization_id` desde o MVP** — DBA (NOT NULL DEFAULT 1), Security, DevOps, Backend e eu convergimos. Isto saiu da zona de "trade-off a apresentar" para **decisão fechada**. Reforço: NOT NULL DEFAULT 1 (DBA/eu) é **superior** a nullable (Security). Nullable convida bugs de "esqueci de setar tenant" e obriga toda query a `WHERE tenant_id = ? OR tenant_id IS NULL`. Security deveria alinhar para NOT NULL.
2. **Envelope encryption (AES-256-GCM + DEK + KEK externo)** para chaves de provedor — Security, DevOps, DBA e eu, unânimes. Fechado.
3. **`pgx` direto, NÃO Beego ORM** — Backend e DBA convergem comigo. Beego ORM descartado.
4. **Routing Engine como pipeline de políticas / ErrorParser registry** — Backend chegou independente ao `parserRegistry[provider_id]` e ao `Selector`/`Chain`/`Forwarder` separados. Convergência forte com meu ADR-002 e ADR-005.
5. **Hexagonal com `domain/` puro** — Backend detalhou o layout quase idêntico ao meu esboço. Convergência total.

Não há o que debater aqui. Passo ao que **diverge**.

---

## 2. Tensões e divergências técnicas (o debate real)

### 2.1 Beego no hot path — concedo parcialmente ao Backend, sustento uma ressalva residual

**Minha posição Turn 1 (ADR-007):** "handler HTTP puro, **não** Controller Beego, isolado do framework".

**Posição do Backend (§3.4, §10.3):** Controller Beego (`proxy/handler.go`) que escreve **direto no `ResponseWriter`**, bypassando `ServeJSON`.

**Minha atualização:** Backend está **correto na prática** — Beego expõe `this.Ctx.ResponseWriter` (um `http.ResponseWriter` real que implementa `http.Flusher`), então escrever chunks SSE de dentro de um Controller funciona. Minha prescrição de "handler totalmente fora do Beego" era **ligeiramente excessiva**. Concedo: um Controller Beego que bypassa `ServeJSON` é aceitável e economiza o custo de manter dois sistemas de roteamento.

**Risco residual que sustento (e Backend não endereçou):** o problema real **não é o Controller, é o servidor HTTP por trás dele**. Beego's `web.Server` (beego v2) tem `Server.WriteTimeout` default que **mata streams longos**. DevOps capturou isto corretamente (§8.2: `WriteTimeout: 0` mandatório). Então a condição de viabilidade do streaming é tripartida:
- Backend: bypass `ServeJSON`, escrever no `ResponseWriter` + `Flusher.Flush()`. ✅
- DevOps: `WriteTimeout=0`, `ReadHeaderTimeout` curto, ingress com `proxy-buffering: off`. ✅
- Arquiteto (eu): documentar que **qualquer filter Beego inserido na chain antes do handler NÃO pode bufferizar** — `web.InsertFilter` roda no estágio `BeforeRouter`, e um filter mal implementado que lê o body de response quebra o stream. **Teste de aceitação obrigatório desde a Fase 3:** `curl -N -d '{"stream":true,...}'` deve receber chunks em tempo real; se bufferizar, o culpado é config de servidor ou filter, não o Controller.

**Veredito:** aceito a abordagem do Backend. Minha ADR-007 deve ser **refinada** para "Controller Beego com bypass de serialização + config de servidor streaming-aware + contrato de filters não-buferizantes" — não "handler fora do Beego".

### 2.2 Graceful shutdown no MVP — reforço contra a postura "MVP+1" do DevOps

**DevOps (§3.3, §10.2):** "puxar graceful shutdown para MVP+1; mitigação = `terminationGracePeriodSeconds: 60`".

**Minha posição:** **Mantenho: graceful shutdown ENTRA NO MVP.** O custo de NÃO ter é desproporcional ao custo de ter.

**Custo de NÃO ter (DevOps subestimou):**
- `terminationGracePeriodSeconds: 60` **não drena** — apenas dá até 60s para conexões terminarem *naturalmente* antes do SIGKILL. Ele **não** (a) para de aceitar novas conexões gracefully, (b) espera apenas in-flight do listener correto, (c) notifica o cliente. Na prática, durante um RollingUpdate, o pod em terminação **continha aceitando requests** até o SIGTERM ser processado, e requests que chegam na janela de terminação são cortados.
- Para um produto cuja **proposição de valor é resiliência transparente**, um chat que para no meio a cada deploy **erosiona a confiança** no produto — exatamente o oposto do critério de sucesso ("failover transparente").
- O produto é de chat com streaming: streams SSE duram **minutos**. Janela de 60s pega streams longos no meio.

**Custo de TER (baixo):**
- ~50-80 linhas de Go: `signal.Notify(SIGTERM)`, `srv.Shutdown(ctx)` com `context.WithTimeout(30s)`, um `sync.WaitGroup` contando streams em flight, listener que para de `Accept()`.
- Sem novas dependências.

**Decisão:** graceful shutdown é **tarefa da Fase 3 (Proxy Core)**, não Fase 10. É barato, o produto é de streaming, e o ROADMAP já o lista — não há motivo para empurrar. DevOps deveria concordar dado que o custo é trivial.

### 2.3 Store abstraction (Redis-ready) — meio-termo pragmático entre DevOps e Backend/eu

Esta é a **tensão mais importante** do round.

**DevOps (§9):** "implementar `Store` interface + `memoryStore` + `redisStore` no MVP; prod com 2 réplicas + Redis". Justificativa: rate limit in-memory com N réplicas = limite × N (falha de governança — o propósito do produto).

**Backend e eu (Turn 1):** single-replica in-memory, Redis no futuro.

**Análise da tensão:** DevOps tem **razão no diagnóstico** mas está **preso a uma premissa questionável** — que prod deve ter `replicas: 2`. Para ~30 usuários internos, 1 réplica + restart rápido do K8s (segundos) é HA aceitável. A premissa "prod = 2 réplicas" é um dogma de HA genérico que não se aplica a esta escala.

**Meio-termo que proponho (e peço endosso):**

| Decisão | Detalhe |
|---|---|
| **`Store` interface no código desde o dia 1** | Concedo ao DevOps — custo ~zero, prepara ROADMAP. ✅ |
| **Apenas `memoryStore` implementada no MVP** | `redisStore` fica como stub não-implementado (não importar `go-redis` ainda — evita dep fantasma). |
| **MVP prod = 1 réplica** | Desafio o dogma "2 réplicas". Para 30 usuários, 1 réplica + `livenessProbe` agressivo + K8s restart (< 10s) é HA suficiente. Isto **elimina inteiramente** a classe de bugs de estado distribuído (rate limit duplo, afinidade fragmentada, circuit breaker divergente). |
| **Gatilho documentado para 2ª réplica + Redis** | Quando: (a) HA virar requisito duro (SLA contratual), OU (b) RPS agregar > ~50/s sustentado, OU (c) feature de quotas distribuídas entrar. Aí: implementar `redisStore`, subir para 2 réplicas, PDB. |
| **Helm chart** | `replicas: 1` default, com comentário explícito: "Scaling beyond 1 replica requires Redis (see ROADMAP) — rate limit and affinity are in-memory." |

**Por que isto é melhor que a proposta do DevOps:** não mentimos sobre HA (1 réplica é honestamente "best-effort, fast-restart"), não introduzimos Redis (deps, ops, latência) para um problema que 1 réplica torna inexistente, e o `Store` interface preserva a evolução. DevOps assume complexidade (`redisStore` + Redis no chart + EgressPolicy p/ Redis) que **só se justifica se houver >1 réplica** — e eu nego essa premissa para o MVP.

**Concessão ao DevOps:** se o time **decidir** que prod deve ser 2 réplicas (decisão de produto, não técnica), então Redis é **mandatório** para rate-limit — concordo com DevOps integralmente nessa bifurcação. A bifurcação é: (1 réplica, sem Redis) OU (≥2 réplicas, Redis obrigatório). **Não existe** "2 réplicas sem Redis" — isso é uma falha silenciosa de governança.

### 2.4 Organização de pacotes — concordo com Backend, uma refinação

Backend propôs hexagonal detalhado. **Concordo** — não é over-engineering para este sistema porque há 4+ adapters (Postgres, N provedores HTTP, Beego, futuro Redis/Prometheus/OAuth). O custo são algumas interfaces; o benefício é testabilidade.

**Refinação (não divergência):** Backend colocou `model_router.go` em `proxy/`. Eu moveria a **lógica de decisão de roteamento** para um pacote `routing/` dedicado, mantendo `proxy/` só como camada de transporte (forward + stream). Razão: o ROADMAP faz o roteamento virar complexo (auto-routing por capacidade, cross-provider failover, quotas, horário). Se `model_router` vive em `proxy/`, ele vira ímã para lógica de negócio que não é de transporte. Isto é **nomeclatura**, não arquitetura — mas evita que `proxy/` vire um pacote-lixo. Backend provavelmente concorda.

### 2.5 Schema do DBA — concordo no núcleo, divirjo em 2 pontos

DBA produziu o schema mais sólido do round. Convergência forte. **Duas divergências:**

**(a) Particionamento de `usage_logs` desde o dia 1 — concedo, com ressalva.**

DBA (§3): particionamento mensal + pg_partman + retention via DROP. Minha reação inicial: prematuro para 30 usuários (~1M linhas/ano, trivial para PG não-particionado).

**Após reflexão: concedo ao DBA.** Razão: o ROADMAP (times, multi-tenancy, quotas) é **multiplicador de volume**. Migrar tabela não-particionada grande para particionada depois é migração online dolorosa (horas, lock, downtime). O custo de particionar agora (pg_partman + CronJob de pre-criação) é **menor** que o custo de migrar depois **dado o vetor de crescimento**. DBA acertou ao olhar a trajetória, não o snapshot.

**Ressalva que sustento:** a automação de **pre-criação de partições futuras** é um **invariante operacional crítico** — se faltar a partição do próximo mês, INSERT falha em produção. Isto deve ser (i) alerta no Prometheus (`missing_partition`), (ii) CronJob K8s idempotente criando 2 meses à frente, (iii) teste no CI. DBA mencionou, mas subdimensiono a criticidade: **é um SPOF operacional silencioso**. Documentar como runbook de prioridade alta.

**(b) Tabelas "vazias" no MVP schema (teams, quotas, time_window_limits) — divirjo, proponho deferir.**

DBA (§6.1): criar `teams`, `team_members`, `quotas`, `time_window_limits` vazias no MVP "porque tabela nova agora ou depois é o mesmo esforço".

**Minha divergência:** criar tabelas **sem código que as lê/escreve** é **schema morto**. Problemas:
- Dá falsa impressão de "feature pronta" (não está).
- Pode **divergir** do que a feature realmente precisará quando for especcada (e.g., quotas podem precisar de janela rolling, não daily; teams podem precisar de hierarquia).
- Adiciona ruído ao schema, migrações, e entendimento de novos devs.

**Regra que proponho (refinamento da §6.3 do DBA):** criar no MVP apenas o que **(i)** toca tabelas quentes particionadas/high-churn (`usage_logs`, `provider_keys`), **OU** (ii) é **coluna única** em tabela existente. Tables **standalone** sem código = deferir.

| Criar no MVP (DBA + eu concordamos) | Deferir do MVP para spec da feature (eu desafio DBA) |
|---|---|
| `organization_id` em todas as multitenant ✅ | `teams`, `team_members` (standalone) |
| `model_aliases` (toca roteamento hot path) | `quotas`, `time_window_limits` (standalone) |
| `model_pricing` + `cost_usd` em usage_logs (coluna) | — |
| `cached_tokens` em usage_logs (coluna) | — |
| `context_window` em models (coluna) | — |

DBA pode contra-argumentar que "criar tabela vazia é grátis". É — mas **manter** schema vivo e alinhado com código não é. Peço deferimento até a feature ser especcada.

---

## 3. Lacunas e erros que identifiquei nos pares

### 3.1 Frontend vs DevOps — contradição direta sobre imagem base (DevOps está certo)

**Frontend (§7):** "`FROM gcr.io/distroless/static` (binário Go estático, sem shell, sem CVEs)".

**DevOps (§2.1):** "`base-debian12:nonroot`, NÃO `static` — o proxy fala HTTPS com provedores → precisa de CA certificates que só `base` traz".

**Veredito:** **DevOps está correto; Frontend errou.** `distroless/static` **não tem** `/etc/ssl/certs/ca-certificates.crt`. O proxy faz **chamadas HTTPS de saída** para Gemini/Mistral/DeepSeek/etc. Sem CA bundle, **todo TLS verify falha** → ou o proxy quebra, ou alguém seta `InsecureSkipVerify: true` (catastrófico, Security §3.4 proíbe). Frontend deve **alinhar para `base-debian12:nonroot`**. Isto é um bug latente se não for corrigido agora.

### 3.2 Security vs realidade de RPS — "sem cache de token" é arriscado para uso agentic

**Security (D4, §4.4):** "MVP sem cache positivo de token (DB hit por request)". Justificativa: revogação instantânea garantida; 30 usuários é trivial.

**Minha crítica:** Security enquadra mal a carga. "30 usuários" ≠ "30 RPS". Um **cliente agentic** (coding agent, research agent) iterando pode gerar **dezenas de RPS por usuário**. DB hit por request, mesmo indexado (~1ms), em pico de 100 req/s = 100ms/s de DB só em validação de token — e DB também está fazendo batch inserts de usage logs, scheduler queries, etc. É contenção evitável.

**Proposta de síntese (entre Security e eu):**
- Cache positivo **curto (15s)** com invalidação ativa.
- `tokenCache` chaveado por `key_id`; entry = `{user, status, expires_at}`.
- **Purge explícito** quando admin desabilita usuário/revoga token: `tokenCache.PurgeUser(userID)` + (multi-pod) pub/sub Redis. Para MVP 1-pod, purge local basta.
- Resultado: revogação é **quase instantânea na prática** (admin clica → purge → próxima request bate DB → rejeitada), latência de validação cai a ~0 no steady-state.

A janela de "revogado mas ainda válido até 15s" é aceitável para ferramenta **interna** com gatilho de purge explícito. **Security deveria aceitar este trade-off** — puro DB-hit é defensável mas não ótimo. Apresento como trade-off, não como bloqueio.

### 3.3 Backend — afinidade por hash tem falso-positivo subdimensionado

Backend (§5.2-5.3) propõe: hash do prefixo como fallback quando não há `X-Conversation-ID`. Reconhece "falsos positivos" mas os subdimensiona.

**Cenário realista de falso-positivo:** dois usuários (ou o mesmo, em sessões distintas) iniciam conversas com o **mesmo system prompt + mesma primeira pergunta canônica** (comum: "Explique X", com system prompt padrão da empresa). O hash do prefixo de 1 mensagem casa → o segundo usuário é roteado para a chave "afim" do primeiro. **Isto não quebra corretude** (a chave funciona), mas:
- derrota o propósito do LRU (concentra carga numa chave por razão errada),
- pode enviar para uma chave que está em cooldown (Backend trata isto, OK),
- vaza **afinidade cruzada entre usuários** se o store não for keyado por `user_id` (Backend keya por user, ✅).

**Minha ênfase:** o `X-Conversation-ID` (Option 1 do Backend) deve ser o **mecanismo primário e documentado como contrato**; o hash é fallback puro com observabilidade de hit-rate. Se hit-rate do hash for alto (indicando muitos clientes sem header), isso é **sinal de que vale a pena pedir aos clientes que adotem o header**, não de que o hash está "funcionando". Convergência com Backend, mas elerio a prioridade do header.

### 3.4 Lacuna coletiva — contrato de compatibilidade OpenAI não foi definido por ninguém

Backend (P1) assume "todo provedor é consumido via schema OpenAI, sem tradução". Isto é **majoritariamente verdade** (Gemini tem modo openai-compat, DeepSeek/Mistral/Z.ai/Kimi são nativos openai-compat) **mas não é 100%**:
- `stream_options.include_usage` — suporte varia (Backend §9.2 admite).
- Campos de `usage` — nomes/ausência variam.
- Formato de erro — varia (motiva o ErrorParser registry).
- `n>1`, `logprobs`, `function_calling` vs `tool_calls` — suporte heterogêneo.

**Ninguém produziu o documento de "contrato de compatibilidade"** — quais endpoints, quais campos do request são passados ipsis litteris, quais features são best-effort, qual o shape de erro devolvido ao cliente. Isto é **tarefa de especificação obrigatória**. Sem ele, "repasse transparente" é ambíguo e vira fonte de bugs. Reforço meu ponto da Turn 1: **contrato de compatibilidade = entregável do spec**.

### 3.5 Lacuna do Backend/DevOps — affinity store não é thread-safe por design explícito

Nenhum par explicitou a **semântica de concorrência** do affinity store sob múltiplas requisições do mesmo usuário em paralelo. Cenário: usuário dispara 2 requests quase simultâneos, ambos com mesmo prefixo, ambos procuram afinidade, ambos não encontram (primeira vez), ambos rodam LRU, ambos gravam entrada — possível **race** onde um sobrescreve o outro. Corretude mantida (ambos usam chaves válidas), mas afinidade "perde" uma.

**Mitigação (acrescento ao spec):** affinity store deve ser **idempotente por conversa** — gravar `(user, conv_id) → key` é upsert; múltiplos writers concorrentes convergem para um valor (o último vence, e qualquer um é válido). Isto é **aceitável** (afinidade é otimização). Mas a semântica deve ser **explícita** e testada. Backend usou `sync.RWMutex` (§5.5) — suficiente. Documentar que "último writer vence" é o contrato.

---

## 4. Onde atualizo meu pensamento (concessões honestas)

| Posição Turn 1 | Atualização Turn 2 | Por quê |
|---|---|---|
| "Handler HTTP puro fora do Beego" | "Controller Beego com bypass de `ServeJSON` + config servidor streaming-aware" | Backend mostrou que `Ctx.ResponseWriter` é um `http.ResponseWriter` real; isolamento no nível de serialização é suficiente. |
| "Postergar particionamento de usage_logs" | **Conceder a DBA:** particionar desde dia 1 | Vetor de crescimento do ROADMAP justifica; migrar depois é caro. |
| "tenant_id NOT NULL DEFAULT default_org (Opção A)" | Mantido, **endurecido** como decisão fechada | Consenso de 5 pares; nullable (Security) é inferior. |
| "ORM: sqlc ou GORM" | **Inclinar para pgx puro + sqlc** | Backend e DBA endossam pgx; sqlc dá type-safety sem ORM magic. |
| "OpenTelemetry no MVP" ( Turn 1 não mencionei) | **Endosso DevOps:** instrumentação OTel desde MVP, collector depois | Custo baixo, essencial depois. |

## 5. O que sustento contra pares

| Posição | Quem divirge | Sustento porque |
|---|---|---|
| **Graceful shutdown no MVP (Fase 3), não MVP+1** | DevOps | Custo ~50 linhas; produto é streaming; `terminationGracePeriodSeconds` não drena de verdade. |
| **MVP prod = 1 réplica (não 2)** | DevOps | 30 usuários internos; 1 réplica elimina classe inteira de bugs de estado distribuído; dogma "2 réplicas" não se aplica. |
| **`redisStore` como stub, não importar go-redis no MVP** | DevOps (quer Redis no MVP) | 1 réplica torna Redis desnecessário; dep fantasma sem uso. |
| **Deferir `teams`/`quotas`/`time_window_limits` (standalone vazias)** | DBA | Schema morto diverge do código; criar quando feature for especcada. |
| **Cache de token 15s + purge explícito > DB-hit por request** | Security | RPS agentic invalida a premissa "30 usuários = baixa carga"; DB-hit é latência evitável. |

---

## 6. Síntese para o consenso

**Decisões fechadas (consenso unânime ou quasi):**
1. Hexagonal + `domain/` puro + `pgx` direto (sem Beego ORM).
2. `organization_id BIGINT NOT NULL DEFAULT 1` em todas as multitenant.
3. Envelope encryption AES-256-GCM + KEK externo (ESO/Sealed Secrets).
4. Routing Engine = pipeline de políticas; ErrorParser = registry por provider.
5. `usage_logs` particionada mensalmente desde dia 1 + automação de pre-criação como invariante operacional.
6. Sessão server-side para UI web (não JWT); `sk-...` só para API proxy.
7. Server-rendered (Beego + HTMX) para UI — não SPA.
8. Imagem `distroless/base-debian12:nonroot` (Frontend deve corrigir de `static`).
9. Particionar `usage_logs`; golang-migrate; managed DB com PITR.

**Decisões a levar à votação/Manager:**
1. **Graceful shutdown: MVP (Fase 3) vs MVP+1.** Eu voto MVP.
2. **Réplicas em prod: 1 (sem Redis) vs 2+ (com Redis).** Eu voto 1 para MVP, com gatilho documentado.
3. **Cache de token: 15s+purge vs DB-hit por request.** Eu voto 15s+purge.
4. **Tabelas standalone vazias no MVP: criar vs deferir.** Eu voto deferir (exceto as que tocam tabelas quentes).
5. **Contrato de compatibilidade OpenAI: produzir como entregável do spec.** Eu voto sim, obrigatório.

**Posição final do Arquiteto:** o design converge fortemente. Os ajustes são de **escopo de MVP** (quanto fazer agora vs deferir), não de **direção arquitetural**. As 5 análises são complementares e juntas cobrem o espaço de decisão melhor que qualquer uma isolada. Recomendo ao Manager **não reabrir** as decisões fechadas e **focar a fase de specificação** em resolver as 5 votações acima + produzir o contrato de compatibilidade OpenAI.