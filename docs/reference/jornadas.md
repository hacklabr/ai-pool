# Jornadas dos usuários — AI Provider Pool (documento vivo)

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.
> Documento vivo: descreve as jornadas **como existem hoje** no produto.
> Atores: **Consumidor da API** (role `user`), **Administrador** (role `admin`),
> **Operador de plataforma** (deploy/infra).

## Jornada 1 — Consumidor da API

**Objetivo:** sair de zero até consumir LLMs pelo pool em minutos, sem nunca ver
uma chave de provedor.

### 1a. Onboarding via convite (caminho principal)
1. Recebe do admin um comando pessoal e de uso único:
   `curl -fsSL https://ia.hacklab.com.br/invite/inv-XXXXXXXX-... | bash`.
2. O script (idêntico para qualquer token — não revela validade):
   - pede senha (mín. 12 caracteres) e confirmação;
   - pergunta se quer configurar **OpenCode** e/ou **MiMo-Code** automaticamente.
3. O script chama `POST /api/web/invite/{token}/accept` (rate-limitado por IP).
   O servidor, em transação: valida o convite (hash SHA-256, pending, não
   expirado — 24 h), cria o usuário (Argon2id) e gera um token `sk-aipp-...`
   (nome "opencode"), retornando o catálogo de modelos ativos.
4. Se aceito, o script grava o provider `llm-pool` em
   `~/.config/opencode/opencode.json` e/ou `~/.config/mimocode/mimocode.json`
   (`baseURL: <pool>/api`, `apiKey: sk-aipp-...`, modelos com `modalities` e
   `limit` context/output).
5. O token é exibido **uma única vez** (prefixo mascarado) — o usuário o guarda.
6. Abre o OpenCode e usa. Pronto.

### 1b. Onboarding manual (alternativa)
1. Faz login em `/web/login` (multi-step; senha criada via convite ou admin).
2. Em **Meus Tokens** (`/web/me/tokens`) cria um token com rótulo; copia o valor
   completo exibido uma única vez.
3. Configura qualquer cliente OpenAI-compatible: `baseURL = <pool>/v1` (ou `/api`),
   `apiKey = sk-aipp-...`.

### 1c. Uso contínuo
1. O cliente chama `/v1/chat/completions` com o nome do modelo cadastrado.
2. O pool valida o token (revogação imediata), aplica rate limit
   (**60 req/min**; excesso → 429 + `Retry-After`), resolve a chave:
   - se o cliente envia `X-Session-Id`/`X-Conversation-ID` (o OpenCode envia),
     a mesma conversa volta à mesma chave → **cache de contexto do provedor**
     (economia); o header `X-Context-Cache: HIT|MISS` confirma.
3. Se uma chave falha (quota, auth, 5xx), o failover tenta a próxima
   **transparentemente**, antes de qualquer byte chegar ao cliente.
4. Erros legítimos do provedor chegam no formato original — o SDK retenta 429
   sozinho, como faria direto no provedor.
5. O usuário acompanha o próprio consumo em **Meu Uso** (`/web/me/usage`) e pode
   revogar/rotacionar seus tokens a qualquer momento.

**Pontos de dor cobertos:** nunca precisa de chave de provedor; não fica sem
cota se outra chave tem saldo; não configura nada à mão se aceitar o setup.

## Jornada 2 — Administrador

**Objetivo:** operar o pool: capacidade (provedores/chaves/modelos), pessoas e
visibilidade de uso.

1. **Primeiro acesso (bootstrap):**
   - Produção: `kubectl exec deploy/<release> -- /aipp
     --bootstrap-admin-username=admin --bootstrap-admin-password='<≥12 chars>'`
     (idempotente). Dev: variáveis `AIPP_BOOTSTRAP_ADMIN_*` no compose.
   - Login em `/web/login` (protegido: rate limit por IP; timing equalization
     anti-enumeração).
2. **Cadastrar capacidade:**
   - **Provedor**: nome, slug, `base_url` (validada contra SSRF), User-Agent
     global opcional.
   - **Modelo**: nome real no provedor, display name, janela de contexto,
     limite de output, modalidades.
   - **Chave**: rótulo + valor — cifrado imediatamente (AES-256-GCM) e nunca
     mais exibido.
   - **Testar**: botão "testar" (uma chave ou todas) faz chamada real
     `max_tokens=1` e atualiza status (200 reabilita; 401 → `auth_failed`;
     429 → `quota_exceeded` com reativação agendada).
3. **Convidar usuário:** em Usuários → criar convite (só o username) → copiar o
   comando `curl | bash` exibido uma única vez → enviar ao usuário. Acompanha
   status pending/used; revoga convites não usados (expiram em 24 h).
4. **Gerir pessoas e acessos:** listar usuários, ver detalhe, resetar senha,
   desabilitar/reabilitar (efeito imediato no próximo request; não pode
   desabilitar a si mesmo); listar/revogar tokens de qualquer usuário.
5. **Acompanhar saúde do pool:** tela de Chaves mostra estados
   `enabled / auto_disabled / manual_disabled`, motivo, contadores de erro e
   `disabled_until`; pode forçar disable/enable. Chaves se auto-desabilitam
   (5 erros/2 min) e reativam sozinhas na data certa (scheduler 30 s + lazy).
6. **Uso e custos:** Dashboard com KPIs (requests, tokens, **cached_tokens** —
   economia de cache, erros) e gráficos por usuário/provedor/modelo em janela
   de tempo. (Custo em dólar: pavimentado, ainda não calculado — ROADMAP.)
   Para investigação com filtros combinados, a tela **Estatísticas**
   (`/web/admin/usage`) cruza período (presets 24h/7d/30d ou datas, máx. 90d),
   usuário, chave de provedor, provedor e modelo — KPIs, série temporal e
   breakdowns por usuário/chave/modelo. Janela limitada à retenção dos logs
   (90 dias); visões de longo prazo usam o Dashboard (rollups).
7. **Auditoria:** tela append-only com filtros — hoje cobre apenas ações de
   convites (cobertura parcial é dívida conhecida).

## Jornada 3 — Operador de plataforma (deploy e operação)

1. **Deploy local (teste):** `cp .env.docker.example .env.docker` +
   `docker compose up --build` → UI em `:8088/web/login`, admin seedado;
   migrations rodam automaticamente em DEV_MODE.
2. **Deploy produção (Helm):**
   - Gera **KEK (32 B) + pepper (48 B)** uma única vez, guarda em cofre offline
     ("perdeu = perdeu as chaves cifradas") e materializa o Secret
     `<release>-secrets` (o chart suporta External Secrets Operator, mas a
     produção atual usa Secret manual — ver ADR-0010).
   - Se registry privado: cria pull secret (`ghcr-pull`).
   - `helm install` sobe Deployment (1 réplica, graceful shutdown,
     RollingUpdate zero-downtime) + PostgreSQL StatefulSet embutido (senha
     auto-gerada, PVC preservado com `resource-policy: keep`) — ou aponta para
     DB gerenciado via `postgresql.enabled=false` + Secret com `DATABASE_URL`.
   - **Migration Job** (post-install hook) aguarda o banco e roda
     `aipp --migrate-only` (golang-migrate, embed.FS) — schema nunca sobe no
     startup do app em produção.
   - Smoke test: `/healthz`; bootstrap do admin; cadastro inicial na UI.
3. **Operação contínua:**
   - **Upgrades/rollback:** `helm upgrade` / `helm rollback`; migrations no hook;
     streams de chat não são cortados (graceful shutdown + preStop sleep).
   - **Partições de `usage_logs`:** garantir partições futuras (pré-criadas só
     até 2026-09; partição `default` é o alarme). Retenção (DROP aos 90 dias) e
     automação via CronJob **não implementadas** — hoje é procedimento manual.
   - **Rotação de senha do PostgreSQL:** `helm upgrade --set
     postgresql.auth.password=...` + restart do pod (Secret regenerado em sync).
   - **Rotação de KEK (anual sugerida, manual):** registrar a KEK nova com novo
     `encryption_key_id`, re-cifrar cada chave (`Decrypt(KEK_antiga)` →
     `Encrypt(KEK_nova)` → UPDATE) — procedimento documentado em
     `internal/adapters/crypto/envelope.go`; decifragem antiga continua
     funcionando durante a transição (múltiplas KEKs coexistem).
   - **Observabilidade:** logs JSON sanitizados → Loki/Grafana (recomendado);
     `/metrics` Prometheus disponível sob feature flag `ENABLE_METRICS=true`;
     dead-letter de usage_logs monitorável (`resolved_at IS NULL`).
   - **Desastre/reset:** `helm uninstall` preserva StatefulSet/PVC do Postgres;
     reset exige deleção explícita. Backup do volume via Velero (opcional);
     DB gerenciado com PITR é o default recomendado.

## Anti-jornadas (o que o sistema impede hoje)
- Consumidor **não consegue** ver chave de provedor nem token de outro usuário
  (anti-IDOR, mascaramento, HMAC).
- Atacante **não consegue** enumerar usuários no login (timing equalization) nem
  validar tokens de convite pelo script (resposta uniforme).
- Admin **não consegue** se trancar fora (não desabilita a si mesmo) nem apontar
  provedor para rede interna/metadata cloud (SSRF guard).
- Um dump do DB **não compromete** chaves (AES-256-GCM + KEK externa) nem tokens
  (HMAC + pepper).
