# Retro of round R02 — Estatísticas de plano das chaves de provedor

## Round signals
- **Specification gaps:** 0
- **New requirements discovered in Stage 3:** 1 — RF-05 (calibração empírica
  da cota) nasceu de ideia do humano durante a revisão do plano; incorporada
  ao escopo antes da aprovação. Não é gap de especificação, é enriquecimento.
- **Late infeasibilities:** 0 (a inexistência de API de cota nos dois
  provedores foi detectada na investigação, antes de qualquer desenho)
- **Documentation contradictions (`doc-bug`):** 0
- **Registered overrides:** 0
- **Feedback:** 1 — contestação do humano corrigindo a classificação da Kimi
  (Membership/Kimi Code dá acesso à API via `api.kimi.com/coding/v1`)

## Process learnings
- Pesquisa web sobre provedores chineses exige checar **os dois domínios e o
  produto consumidor** (kimi.com/code ≠ platform.kimi.com ≠ platform.kimi.ai):
  a primeira rodada de pesquisa olhou só a plataforma de API e concluiu errado.
  Em demandas futuras de investigação de provedor, a pergunta "existe um
  produto consumidor/coding-plan separado da plataforma de API?" deve constar
  do roteiro.
- A evidência viva do ambiente (o próprio modelo servindo a sessão via o plano
  investigado) é um sinal de correção forte — ouvir o humano na primeira
  contestação economizou um round inteiro de desenho errado.
