## Briefing: AI Provider Pool - OpenAI-Compatible Proxy

**LEIA O BRIEFING COMPLETO no arquivo:**
`/home/rafael/devel/ai-pool/.mesa/briefings/briefing-ai-provider-pool.md`

Use a ferramenta `read` para ler o arquivo completo antes de analisar.

### Resumo rápido do projeto
Um pool/load-balancer de provedores de LLM, compatível com a API OpenAI, que funciona como proxy inteligente. Stack: **Go + Beego + PostgreSQL**, deploy em **Docker Compose (local)** e **Kubernetes + Helm (produção)**. Licença MIT.

### Escala
~30 usuários, ~5 provedores (Gemini, Mistral, DeepSeek, Z.ai, Kimi), 1-5 chaves por provedor.

### Funcionalidades-chave do MVP
1. Proxy OpenAI-compatible (repasse transparente) com override de User-Agent e suporte a streaming
2. Load balancing LRU entre chaves + afinidade de cache de contexto (detecção de prefixo de conversa)
3. Failover dentro do mesmo provedor (tenta todas as chaves) + retry com backoff exponencial
4. Auto-disable de chave com parse de data (reativada na data informada pelo provedor)
5. RBAC (admin vs usuário comum) + gestão de usuários
6. Autenticação da API via token (estilo sk-...)
7. Rate limiting por usuário/token
8. Gestão manual de chaves (admin força desabilitar/habilitar)
9. Registro de uso/auditoria por requisição
10. Interface web (gestão + dashboard de utilização)

### Roadmap (decidir AGORA para facilitar o futuro)
Modelo "auto" cross-provider, times, quotas de tokens, limitação de horário, multi-tenancy, OAuth, Prometheus metrics, model aliasing, health checks, webhooks, custo por token, graceful shutdown.

### Atenção especial dos especialistas
- Extensibilidade é crítica (pavimentar teams/quotas/tenancy/auto-routing)
- Parsing de mensagens de erro é frágil — design extensível por provedor
- Lógica de afinidade de cache de contexto é diferencial do produto