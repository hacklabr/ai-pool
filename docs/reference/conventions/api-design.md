# Convenção — API design

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

Fonte: `internal/adapters/httpserver/` (proxy + admin).

## Proxy (`/v1/*`, `/api/*`) — OpenAI-compatible

- Compatibilidade estrita com a API OpenAI: clientes trocam apenas `baseURL` + `apiKey`.
- **Body do cliente nunca é re-serializado** — bytes originais vão ao upstream
  (preserva cache de contexto e campos desconhecidos).
- Erros **do provedor** são repassados no formato original; erros **do proxy**
  usam o envelope OpenAI (`internal/adapters/httpserver/proxy/errors.go`).
- Headers de diagnóstico: `X-Context-Cache: HIT|MISS`; rate limit responde
  `429 + Retry-After`.
- Auth: `Bearer sk-aipp-...`; 401 genérico (anti-enumeração).

## Admin API (`/api/web/*`)

- Envelope de erro único: `{"error":{code,message}}` via `writeAPIError`.
- Parse dual: JSON (`DisallowUnknownFields` + `MaxBytesReader`) com fallback a
  form-urlencoded (forms HTMX) — regra "campo vazio = omitido".
- Mutations exigem sessão + CSRF double-submit (cookie `aipp_csrf` + header
  `X-CSRF-Token`) + role adequado (RBAC `admin`/`user`).
- Pipeline de middleware na ordem: `csrf → sessionAuth → requireRole` (a ordem
  importa — ver comentário do bug F9 em `admin.go`).
- Endpoints anti-enumeração: respostas uniformes para recurso
  válido/inválido/expirado (convites, login).
- Comparacões de segredos sempre constant-time.
