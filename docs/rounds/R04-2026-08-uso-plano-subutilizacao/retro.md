# Retro of round R04 — Uso do plano por ciclo, calibração e subutilização

## Round signals
- **Specification gaps:** 0
- **New requirements discovered in Stage 3:** 0
- **Late infeasibilities:** 0
- **Documentation contradictions (`doc-bug`):** 0
- **Registered overrides:** 0
- **Feedback:** 0

## Process learnings
- A regressão de teste da R03 (versão de migration esperada) só foi detectada
  na R04 porque o especialista comparou com a base — os testes de integração
  não rodam no `ci-local` (tag `integration`), então quebraram em silêncio.
  Aprendizado: toda migration nova deve atualizar o teste de versão NO MESMO
  commit (verificado na revisão do facilitador), ou o teste deveria derivar a
  versão esperada dinamicamente do FS embedado em vez de hardcode.
- Falha pré-existente `TestInvitationRepo_Accept*` (FK) capturada como stage-0
  em vez de silenciosamente ignorada — rastreamento de dívida funciona.
