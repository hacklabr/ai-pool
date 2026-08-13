---
name: handlers-admin
description: Use quando for criar ou alterar handlers da API/UI admin (/web/*, /api/web/*) — envelope de erro, parse dual JSON/form, middleware e RBAC.
---

# Handlers admin (envelope, parse dual, middleware)

## Pré-requisitos

- Ler `internal/adapters/httpserver/admin/web_helpers.go` e a convenção
  `docs/reference/conventions/api-design.md`.

## Procedimento

1. Erros sempre via `writeAPIError(w, status, code, msg)` — envelope único
   `{"error":{code,message}}`; nunca `http.Error` avulso.
2. Parse de input via `parseJSON` (com `DisallowUnknownFields` + `MaxBytesReader`)
   com fallback a form-urlencoded — a regra é "campo vazio = omitido" (coerção
   form→JSON→struct em `web_helpers.go`).
3. IDs de path via helper `pathID`; IP do cliente via `clientIP`.
4. Pipeline de middleware NA ORDEM: `csrf → sessionAuth → requireRole` — a ordem
   importa (ver comentário do bug F9 em `admin.go:326-332`).
5. RBAC: rotas `/web/admin/*` e `/api/web/admin/*` exigem role `admin`; `/web/me/*`
   para qualquer role com escopo ao próprio usuário (anti-IDOR: validar ownership
   antes de operar).
6. Comparações de segredos sempre constant-time (`subtle.ConstantTimeCompare`).
7. Endpoints sensíveis são anti-enumeração: mesma resposta para recurso
   válido/inválido/expirado.
8. Dependências opcionais do `App`: cheque nil e responda 503 graceful (ver skill
   `di-setter-nil-safe`).

## Critérios de pronto

- [ ] Testes passando (`make test`)
- [ ] Tipos checados (`go build ./...`)
- [ ] Documentação atualizada se o contrato mudou
- [ ] Mutation nova protegida por CSRF + role correto
