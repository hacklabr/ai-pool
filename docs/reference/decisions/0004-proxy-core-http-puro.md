# ADR-0004 — Proxy core em HTTP puro, isolado do Beego

**Status:** aceito

**Contexto:** Frameworks full-stack (Beego) podem bufferizar respostas e quebrar streaming SSE, além de esconder o controle de flush por chunk. O hot path do proxy exige controle fino de `http.Flusher`, timeouts de streaming (first-byte/idle/overall) e repasse chunked transparente.

**Decisão:** O proxy core usa apenas `net/http` + `http.Flusher`, sem Beego no hot path. Beego permanece como placeholder residual (catch-all) para admin/UI; a composição de handlers (`composeHandler`) prioriza Proxy Core > Admin App > Beego. O Ingress deve ter `proxy-buffering: off` para preservar SSE.

**Consequências:** Positivas: streaming SSE previsível e testável; timeouts por fase do stream; superfície mínima no hot path. Negativas: duas stacks HTTP coexistem (custo cognitivo); features de framework (sessão, templates) precisam ser reimplementadas no admin com net/http. Nota: o código já referencia esta decisão como "ADR-007" em comentário — este ADR (0004) é o registro canônico.

**Evidências:** `internal/adapters/httpserver/proxy/server.go`, `stream.go`; `cmd/aipp/main.go` (composeHandler); `deploy/helm/ai-pool/values-prod.yaml` (annotations SSE).

**Origem:** DECISIONS.md A4 (consenso de especialistas 6-0).
