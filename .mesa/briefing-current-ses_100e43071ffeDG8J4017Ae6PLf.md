---
title: "AI Provider Pool - OpenAI-Compatible Proxy"
slug: "ai-provider-pool"
date: "2026-06-25T18:26:25.600Z"
status: approved
---
# AI Provider Pool - OpenAI-Compatible Proxy

## Visão

Desenvolver um **pool/load-balancer de provedores de modelos de IA**, compatível com a API OpenAI, que funciona como um **proxy inteligente** entre aplicações cliente e múltiplos provedores de LLM. O sistema distribui a carga entre várias chaves de API, garante resiliência via failover automático, e centraliza o acesso — eliminando o compartilhamento inseguro de chaves.

## Contexto e Motivação ("Por que isto, por que agora")

- **Dor real:** Membros da equipe ficam sem tokens em seus planos individuais enquanto outros têm saldo de sobra.
- **Problema de processo/segurança:** Chaves de acesso vêm sendo compartilhadas informalmente via Telegram — prática insegura e sem governança.
- **Solução:** Um pool centralizado que distribui a carga, aproveita melhor os saldos disponíveis, e controla o acesso por usuário via tokens próprios.

## Estado Atual

Projeto **greenfield** (do zero). Não existe código prévio nem sistema similar de referência.

## Objetivos

1. Centralizar e governar o acesso a múltiplos provedores/chaves de LLM.
2. Distribuir carga entre chaves do mesmo provedor, maximizando o aproveitamento das cotas disponíveis.
3. Garantir resiliência: falhas em uma chave não devem quebrar a requisição do cliente.
4. Aproveitar **cache de contexto** dos LLMs roteando conversas contínuas para a mesma chave.
5. Desabilitar automaticamente chaves problemáticas, reativando-as quando a cota for liberada.
6. Prover governança via usuários, tokens e RBAC.
7. Oferecer visibilidade de uso via dashboard.

## Critérios de Sucesso

> A equipe utiliza o proxy no dia a dia como camada única de acesso aos provedores de IA, **sem mais troca de chaves via Telegram**, com distribuição de carga funcionando e failover transparente.

## Parâmetros de Escala

- **Usuários:** ~30
- **Provedores:** ~5 (Gemini, Mistral, DeepSeek, Z.ai, Kimi)
- **Chaves por provedor:** 1 a 5

## Restrições Técnicas (Constraints)

| Item | Decisão |
|------|---------|
| Linguagem | **Go** |
| Framework web | **Beego** |
| Banco de dados | **PostgreSQL** |
| Ambiente local/teste | **Docker Compose** |
| Ambiente de produção | **Kubernetes + Helm Chart** |
| Licença | **MIT** |
| Compatibilidade de API | **OpenAI-compatible** (repasse transparente) |

## Escopo do MVP

### Funcionalidades Principais

1. **Proxy OpenAI-compatible (repasse transparente)**
   - Repassa as requisições aos provedores sem alterar o payload.
   - Modifica o **User-Agent** quando um override estiver configurado por provedor/chave.
   - **Suporte a streaming** (`stream: true`) no repasse (essencial para chat).

2. **Balanceamento de carga entre chaves (LRU)**
   - Estratégia: **Least Recently Used** (menos usada recentemente).
   - **Afinidade de cache de contexto:** para o mesmo usuário numa mesma tarefa, se a mensagem anterior estiver contida no início da mensagem atual (detecção de prefixo/continuidade de conversa), o sistema envia para a **mesma chave do mesmo provedor** para aproveitar o cache de contexto do LLM.

3. **Failover dentro do mesmo provedor**
   - Quando uma chave retorna erro, o sistema tenta a **próxima chave do mesmo provedor**, usando o mesmo modelo solicitado.
   - **Tenta todas as chaves do provedor** antes de retornar o erro ao cliente.
   - **Retry com backoff exponencial** entre tentativas.

4. **Auto-disable de chave com parse de data**
   - Chaves que retornam erro com frequência são desabilitadas temporariamente.
   - O sistema faz **parse da mensagem de erro** em busca de uma data (vários provedores informam quando a cota de tokens será liberada).
   - A chave é **reativada automaticamente na data informada** pelo provedor.
   - **Se o parsing falhar**, a chave entra no fluxo de retry com backoff (não bloqueia).

5. **RBAC e gestão de usuários**
   - Cadastro de usuários com login por **usuário + senha**.
   - **Roles:** `admin` (gerencia provedores, chaves globais e usuários) e usuário comum (consome o pool via seus tokens).
   - Usuários podem **cadastrar/gerar seus próprios tokens de API** (estilo `sk-...`).
   - Ao **desabilitar/excluir um usuário**, ele perde acesso ao pool.

6. **Autenticação da API do proxy**
   - A API exige um **token válido** no header `Authorization: Bearer`.

7. **Rate limiting por usuário/token** (ex.: req/min).

8. **Gestão manual de chaves (admin)**
   - Admin pode **forçar desabilitar/habilitar** uma chave manualmente.

9. **Registro de uso e auditoria**
   - Logging por requisição: **quem** usou, **qual** chave/provedor/modelo, **quantos tokens**, **quando**.

10. **Interface web**
    - Gerenciamento de **usuários**, **provedores** e **chaves** (admin).
    - **Dashboard** com gráfico de utilização por usuário.
    - Abordagem técnica (server-rendered vs SPA) a critério dos especialistas.

### Decisões Técnicas Preliminares

- **Go + Beego** como framework principal.
- **PostgreSQL** como persistência.
- **Docker Compose** para desenvolvimento/teste local.
- **Helm Chart** para deploy em Kubernetes.

## Fora de Escopo / ROADMAP

> Os especialistas devem considerar estes itens ao tomar decisões de arquitetura agora, **facilitando a implementação futura**. Serão registrados em arquivo `ROADMAP.md` separado.

- **Modelo "auto"** com roteamento automático cross-provider/modelo por heurística (ex.: arquivos anexados → modelo multimodal).
- **Failover cross-provider** (ex.: Gemini esgotou → cair para outro provedor). *Atenção ao tamanho da janela de contexto, que pode variar entre provedores.*
- **Times**, cada um com acesso a uma coleção de provedores/chaves/modelos.
- **Quota de utilização de tokens** por usuário/time em períodos de tempo.
- **Limitação de horário** de utilização por usuário (ex.: só entre 8h e 18h).
- **Multiorganização (multi-tenancy).**
- **Autenticação OAuth** (configurável qual método usar).
- **Endpoint `/metrics` no formato Prometheus** (observabilidade em K8s).
- **Model aliasing/mapping** (alias `gpt-4` → modelo interno de provedor).
- **Health check periódico das chaves** (probe em background).
- **Notificação/webhook** quando chave é desabilitada.
- **Cálculo/cadastro de custo** por provedor/modelo (preço por token).
- **Graceful shutdown** (drenar conexões ao reimplantar no K8s).

## Notas para os Especialistas

- **Extensibilidade é crítica:** várias decisões de arquitetura agora devem pavimentar o caminho para times, quotas, multi-tenancy e roteamento "auto" no futuro.
- **O parsing de mensagens de erro é frágil** (depende do formato de cada provedor) — projetar de forma que adicionar parsers por provedor seja simples e extensível.
- **A lógica de afinidade de cache de contexto** (detecção de prefixo de conversa) é um diferencial importante do produto e merece atenção especial dos arquitetos.
- **O desenvolvimento será conduzido via este workflow Mesa** (discovery → discussion → specification → execution).
