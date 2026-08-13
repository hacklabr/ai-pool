---
name: sanitizacao-de-logs
description: Use sempre que for escrever ou revisar código que emite logs neste projeto — garante que nenhum segredo (chaves, tokens, KEK, hash de afinidade) vaze em logs.
---

# Sanitização de logs

## Pré-requisitos

- Conhecer o `SanitizingHandler` em `internal/adapters/security/sanitizing_logger.go`.

## Procedimento

1. Use **sempre** o logger construído em `cmd/aipp/main.go` (`buildLogger`) — nunca `slog.Default()` nem um `slog.New` avulso: a proteção só existe no handler sanitizante global.
2. Nomeie atributos de log evitando os nomes sensíveis redigidos pelo handler (`password`, `token`, `kek`, `decrypted_value`…) — se o valor for sensível, simplesmente não o logue; a redação é rede de segurança, não licença.
3. Nunca logue: valores decifrados de chaves, tokens `sk-aipp-...` completos, KEK, pepper, e o **hash de afinidade** (convenção separada: `routing/engine.go` loga apenas a origem — `"header"` ou `"hash"`).
4. Se um novo tipo de segredo surgir (novo formato de token, nova credencial), adicione o padrão ao `SanitizingHandler` (nome de atributo e/ou regex) no mesmo PR.
5. Segredos decifrados em memória: `defer zeroBytes(...)` após o uso.

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Nenhum log novo expõe segredo (revisão própria: grep pelos valores nos call sites)
