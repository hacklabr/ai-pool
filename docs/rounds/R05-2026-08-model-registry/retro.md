# Retro of round R05 — dicionário interno de especificações de modelos

## Round signals
- **Specification gaps:** 0.
- **New requirements discovered in Stage 3:** 0.
- **Late infeasibilities:** 0.
- **Documentation contradictions (`doc-bug`):** 0.
- **Registered overrides:** 0.
- **Feedback:** 0.

## Process learnings
- O backfill foi implementado como método dedicado (`ModelRepo.BackfillSpecs`)
  em vez de reusar `Update`: assim a invariante "nunca sobrescrever" (ADR-0015)
  é garantida pelo próprio SQL (`COALESCE`/`CASE`), não pela disciplina do
  caller — elimina a raça de um admin editando o modelo durante a rodada.
- A porta consumer-side `SpecSource` com nil = comportamento anterior manteve
  todos os testes existentes do modelsync verdes sem edição de semântica — o
  mesmo padrão de injeção do decryptor/identity da R01.
- Dicionário embutido (embed.FS) mostrou-se barato de testar com o registry
  REAL nos testes do modelsync (dados validados exercitam lookup, herança e
  override de alias de ponta a ponta).
