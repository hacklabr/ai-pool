package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/routing"
)

// =============================================================================
// KeyTester — testa chaves de provedores fazendo um /chat/completions real
// com max_tokens=1 e atualizando o status da chave conforme a resposta.
//
// Funcionalmente distinto do auto-disable do routing engine:
//   - Routing engine: acionado em tráfego real, CAS no status, integra LRU.
//   - KeyTester: acionado por admin (UI), autoritativo (atualiza qualquer
//     status), sem efeito colateral no LRU.
//
// Mapeamento de resposta do provedor → ação:
//
//	200 (chat OK)        → enabled (reativa, zera contadores)
//	401/403 (auth)       → auto_disabled + auth_failed (sem disabled_until)
//	429 (rate/quota)     → auto_disabled + quota_exceeded + disabled_until
//	                      (Retry-After: N ou fallback 1h)
//	5xx, timeout, rede   → NÃO muda status (infra do provedor, não da chave)
//	outros 4xx           → NÃO muda status (reporta só)
//
// Segurança:
//   - DecryptedValue NUNCA entra em log; apenas key_id, provider_slug, status.
//   - Body do provedor é sanitizado (truncado, sem newline) antes de expor.
//   - Authorization é sobrescrito — nunca repassamos o token sk-aipp-... do admin.
// =============================================================================

// TestResult é o resultado de um teste individual de chave.
type TestResult struct {
	KeyID        int64  `json:"key_id"`
	Label        string `json:"label"`
	ProviderSlug string `json:"provider_slug,omitempty"`
	OK           bool   `json:"ok"`
	StatusCode   int    `json:"status_code,omitempty"`
	LatencyMs    int64  `json:"latency_ms,omitempty"`
	Message      string `json:"message"`
	NewStatus    string `json:"new_status,omitempty"` // vazio se não mudou
	ErrorHint    string `json:"error_hint,omitempty"` // trecho sanitizado do body
}

// testAllResponse é o envelope retornado por POST /api/web/admin/keys/test-all.
type testAllResponse struct {
	Results []TestResult `json:"results"`
	Summary struct {
		Total  int `json:"total"`
		OK     int `json:"ok"`
		Failed int `json:"failed"`
	} `json:"summary"`
}

// keyTesterImpl implementa o teste de chaves. Cada método é nil-safe para os
// campos opcionais (decryptor, upstreamClient) — handlers checam antes de chamar.
type keyTesterImpl struct {
	app *App
}

// teste de chave: limite de bytes lidos do body do provedor (defesa DoS).
const providerBodyMaxBytes = 8 * 1024

// teste de chave: timeout total por chamada ao provedor (covers DNS+TLS+request).
const testKeyTimeout = 15 * time.Second

// teste de chave: concorrência máxima para "test-all" (evita saturar provedores).
const testAllConcurrency = 5

// reRetryAfterInline captura "retry-after": N em JSON ou header do provedor.
// Regex simplificada (para uso inline no KeyTester — não substitui o parser
// completo do routing engine, que tem mais formatos).
var reRetryAfterInline = regexp.MustCompile(`(?i)retry[-_ ]?after["']?\s*[:=]\s*"?(\d+)`)

// handleAdminTestKey testa UMA chave e atualiza seu status.
//
// Rotas: POST /api/web/admin/keys/{id}/test.
// Autenticação: withAdminAuth (session + CSRF + role=admin).
func (a *App) handleAdminTestKey(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.decryptor == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_configured",
			"key decryptor not configured (set KEK_PATH or DEV_MODE)")
		return
	}
	if a.upstreamClient == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_configured",
			"upstream HTTP client not configured")
		return
	}
	id := pathKeyID(r.URL.Path)
	if id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}

	kt := &keyTesterImpl{app: a}
	res, err := kt.testKey(r.Context(), domain.OrganizationID(sess.OrgID), domain.ID(id), &sess.UserID)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal", "test failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAdminTestAllKeys testa TODAS as chaves da org em paralelo e atualiza
// seus status individualmente. Concorrência limitada por testAllConcurrency.
//
// Rotas: POST /api/web/admin/keys/test-all.
// Autenticação: withAdminAuth (session + CSRF + role=admin).
func (a *App) handleAdminTestAllKeys(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	if a.decryptor == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_configured",
			"key decryptor not configured (set KEK_PATH or DEV_MODE)")
		return
	}
	if a.upstreamClient == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_configured",
			"upstream HTTP client not configured")
		return
	}

	// Lista todas as chaves da org.
	keys, err := a.keyRepo.ListAll(r.Context(), domain.OrganizationID(sess.OrgID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not list keys")
		return
	}

	// Context derivado com timeout folgado: cada teste tem 15s, mas o test-all
	// como um todo pode levar até (n/5)*15s + overhead. Para 20 chaves → 60s.
	// Usamos 5min hard cap para evitar requests pendurados.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	kt := &keyTesterImpl{app: a}
	results := make([]TestResult, len(keys))

	sem := make(chan struct{}, testAllConcurrency)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		go func(idx int, keyID domain.ID) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Cada teste roda com seu próprio sub-context (timeout individual).
			res, err := kt.testKey(ctx, domain.OrganizationID(sess.OrgID), keyID, &sess.UserID)
			if err != nil || res == nil {
				results[idx] = TestResult{
					KeyID:   int64(keyID),
					OK:      false,
					Message: "erro interno ao testar",
				}
				return
			}
			results[idx] = *res
		}(i, k.ID)
	}
	wg.Wait()

	// Resumo.
	var ok, failed int
	for _, res := range results {
		if res.OK {
			ok++
		} else {
			failed++
		}
	}
	var resp testAllResponse
	resp.Results = results
	resp.Summary.Total = len(results)
	resp.Summary.OK = ok
	resp.Summary.Failed = failed
	writeJSON(w, http.StatusOK, resp)
}

// testKey executa um teste individual e persiste o novo status.
// actorUserID identifica o admin que disparou o teste (auditoria, issue #19);
// pode ser nil (ação de sistema).
// Retorna ErrNotFound (postgres) se a chave não existe na org.
func (kt *keyTesterImpl) testKey(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID, actorUserID *int64) (*TestResult, error) {
	app := kt.app

	// 1. Carrega chave.
	k, err := app.keyRepo.GetByID(ctx, orgID, keyID)
	if err != nil {
		return nil, err
	}

	// 2. Carrega provider (para BaseURL + User-Agent global).
	prov, err := app.providerRepo.GetByID(ctx, orgID, k.ProviderID)
	if err != nil {
		return nil, fmt.Errorf("load provider: %w", err)
	}

	// 3. Resolve slug do provider para o result (best-effort, não bloqueante).
	providerSlug := prov.Slug

	// 4. Decifra o valor da chave.
	decrypted, err := app.decryptor.Decrypt(ctx, *k)
	if err != nil {
		return &TestResult{
			KeyID:        int64(k.ID),
			Label:        k.Label,
			ProviderSlug: providerSlug,
			OK:           false,
			Message:      "erro interno: falha ao decifrar a chave",
		}, nil
	}
	// Zeroiza o plaintext o quanto antes (defesa em profundidade).
	defer wipeBytes(decrypted)

	// 5. Busca um modelo ativo deste provider (necessário para o body do /chat).
	modelName, modelErr := kt.firstActiveModelName(ctx, orgID, k.ProviderID)
	if modelErr != nil {
		// Sem modelo ativo: não chama o provedor; reporta e NÃO muda status.
		return &TestResult{
			KeyID:        int64(k.ID),
			Label:        k.Label,
			ProviderSlug: providerSlug,
			OK:           false,
			Message:      "nenhum modelo ativo para este provedor; cadastre um modelo antes de testar",
		}, nil
	}

	// 6. Monta body do /chat/completions.
	body, err := buildTestChatBody(modelName)
	if err != nil {
		return nil, fmt.Errorf("build test body: %w", err)
	}

	// 7. Monta request ao provedor.
	targetURL := strings.TrimRight(prov.BaseURL, "/") + "/chat/completions"
	// #nosec G704 -- targetURL é construído do BaseURL cadastrado (validado por
	// domain.ValidateProviderURL no admin path). Não há taint flow do request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+string(decrypted))
	// User-Agent override: provider global OU por chave.
	ua := prov.UserAgentGlobal
	if k.UserAgentOverride != nil && *k.UserAgentOverride != "" {
		ua = k.UserAgentOverride
	}
	if ua != nil && *ua != "" {
		req.Header.Set("User-Agent", *ua)
	}

	// 8. Timeout individual (sub-context).
	reqCtx, cancel := context.WithTimeout(ctx, testKeyTimeout)
	defer cancel()
	req = req.WithContext(reqCtx)

	// 9. Envia.
	start := time.Now()
	resp, err := app.upstreamClient.Do(req) // #nosec G704 -- targetURL vem do BaseURL cadastrado (validado por domain.ValidateProviderURL no admin path); sem taint do request do cliente.
	latency := time.Since(start)
	if err != nil {
		// Erro de rede/DNS/timeout → infra do provedor, NÃO muda status.
		msg := "falha de rede ao contatar provedor"
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			msg = "timeout ao contatar provedor"
		}
		return &TestResult{
			KeyID:        int64(k.ID),
			Label:        k.Label,
			ProviderSlug: providerSlug,
			OK:           false,
			LatencyMs:    latency.Milliseconds(),
			Message:      msg + " (" + latency.Truncate(time.Millisecond).String() + ")",
		}, nil
	}
	defer resp.Body.Close()

	// 10. Lê body (limitado, sanitizado).
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, providerBodyMaxBytes+1))
	hint := sanitizeHint(respBody)

	// 11. Decide ação conforme status code.
	result := TestResult{
		KeyID:        int64(k.ID),
		Label:        k.Label,
		ProviderSlug: providerSlug,
		StatusCode:   resp.StatusCode,
		LatencyMs:    latency.Milliseconds(),
		ErrorHint:    hint,
	}

	newStatus, reason, disabledUntil, persist := decideTestAction(resp.StatusCode, resp.Header, respBody)
	switch {
	case !persist:
		// Erro de infra (5xx) ou 4xx não mapeado → não mexe no status.
		result.OK = false
		result.Message = formatProviderMessage(resp.StatusCode, latency, hint)
		result.NewStatus = ""
	case newStatus == string(domain.KeyStatusEnabled):
		// 200 OK → reativa.
		if err := app.keyRepo.ApplyTestStatus(ctx, orgID, k.ID, true, "", nil, nil); err != nil {
			result.OK = false
			result.Message = "OK do provedor mas erro ao atualizar status: " + err.Error()
			return &result, nil
		}
		// Auditoria da transição (issue #19, RF-03). Best-effort.
		kt.auditTestTransition(ctx, orgID, k.ID, actorUserID, "provider_key.test_enable",
			resp.StatusCode, "", "", latency)
		result.OK = true
		result.NewStatus = string(domain.KeyStatusEnabled)
		result.Message = fmt.Sprintf("OK · %dms", latency.Milliseconds())
		result.ErrorHint = ""
	case newStatus == string(domain.KeyStatusAutoDisabled):
		// 401/403/429 → desativa.
		if err := app.keyRepo.ApplyTestStatus(ctx, orgID, k.ID, false, reason, disabledUntil, &hint); err != nil {
			result.OK = false
			result.Message = "Falha do provedor mas erro ao atualizar status: " + err.Error()
			return &result, nil
		}
		// Auditoria da transição com a camada do limite + mensagem upstream
		// sanitizada (issue #19, RF-03). Best-effort.
		layer, _ := routing.ClassifyLimitLayer(providerSlug, resp.StatusCode, string(respBody))
		kt.auditTestTransition(ctx, orgID, k.ID, actorUserID, "provider_key.test_disable",
			resp.StatusCode, layer, hint, latency)
		result.OK = false
		result.NewStatus = string(domain.KeyStatusAutoDisabled)
		result.Message = formatDisabledMessage(resp.StatusCode, reason, disabledUntil, latency)
	}

	// Log estruturado: apenas key_id, provider, status. NUNCA decrypted/auth.
	slog.Info("key test",
		"key_id", k.ID,
		"provider", providerSlug,
		"status_code", resp.StatusCode,
		"latency_ms", latency.Milliseconds(),
		"new_status", result.NewStatus,
	)
	return &result, nil
}

// auditTestTransition registra a transição aplicada pelo teste de chave no
// audit_log (issue #19, RF-03). Best-effort: falhas são silenciosas — a
// auditoria não pode derrubar o teste já persistido. errorMessage DEVE vir
// sanitizado (sanitizeHint/SanitizeUpstreamMessage) — nunca contém a chave.
func (kt *keyTesterImpl) auditTestTransition(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	actorUserID *int64,
	action string,
	statusCode int,
	limitLayer string,
	errorMessage string,
	latency time.Duration,
) {
	app := kt.app
	if app.auditRepo == nil {
		return
	}
	kid := int64(keyID)
	meta := map[string]any{
		"status_code": statusCode,
		"latency_ms":  latency.Milliseconds(),
	}
	if limitLayer != "" {
		meta["limit_layer"] = limitLayer
	}
	if errorMessage != "" {
		meta["error_message"] = errorMessage
	}
	_ = app.auditRepo.Append(ctx, postgres.AuditEntry{
		OrganizationID: int64(orgID),
		ActorUserID:    actorUserID,
		Action:         action,
		EntityType:     "provider_key",
		EntityID:       &kid,
		Metadata:       meta,
	})
}

// firstActiveModelName retorna o nome do primeiro modelo ativo do provider.
// Necessário porque o body do /chat/completions exige um campo "model" válido.
func (kt *keyTesterImpl) firstActiveModelName(ctx context.Context, orgID domain.OrganizationID, providerID domain.ID) (string, error) {
	// Query direta no pool — evita adicionar método ao ModelRepo só para isto.
	row := kt.app.pool.QueryRow(ctx, `
		SELECT name FROM models
		WHERE organization_id = $1 AND provider_id = $2 AND is_active = true
		ORDER BY name LIMIT 1`, int64(orgID), int64(providerID))
	var name string
	if err := row.Scan(&name); err != nil {
		return "", fmt.Errorf("no active model: %w", err)
	}
	return name, nil
}

// buildTestChatBody monta o body JSON para um /chat/completions mínimo.
// max_tokens=1 para minimizar consumo de tokens do cliente.
func buildTestChatBody(model string) ([]byte, error) {
	body := map[string]any{
		"model":       model,
		"max_tokens":  1,
		"stream":      false,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	}
	return json.Marshal(body)
}

// decideTestAction mapeia o status code do provedor para a ação de status da chave.
// Retorna:
//   - newStatus: "enabled" | "auto_disabled" | ""
//   - reason: domain.KeyDisabledReason (vazio se newStatus="")
//   - disabledUntil: data de reativação (apenas para 429/quota)
//   - persist: true se deve aplicar a transição (false para 5xx/4xx não mapeados)
func decideTestAction(statusCode int, header http.Header, body []byte) (newStatus string, reason domain.KeyDisabledReason, disabledUntil *time.Time, persist bool) {
	switch {
	case statusCode == http.StatusOK:
		return string(domain.KeyStatusEnabled), "", nil, true
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		// Auth falhou — não há quando reativar (chave inválida até correção).
		return string(domain.KeyStatusAutoDisabled), domain.ReasonAuthFailed, nil, true
	case statusCode == http.StatusTooManyRequests:
		// 429 = rate limit ou quota excedida.
		// Tenta Retry-After do header (padrão HTTP); fallback para regex no body.
		until := parseRetryAfter(header, body)
		if until == nil {
			// Fallback conservador: 1h. Em Cohen com a semântica de quota, o
			// admin pode reativar manualmente quando souber que liberou.
			t := time.Now().Add(1 * time.Hour)
			until = &t
		}
		reason := domain.ReasonQuotaExceeded
		return string(domain.KeyStatusAutoDisabled), reason, until, true
	default:
		// 5xx, outros 4xx → não muda status.
		return "", "", nil, false
	}
}

// parseRetryAfter extrai disabled_until a partir de:
//  1. Header "Retry-After: N" (segundos) — padrão HTTP.
//  2. Regex inline no body ("retry-after": N em JSON).
//
// Retorna nil se não conseguir extrair (caller decide fallback).
func parseRetryAfter(header http.Header, body []byte) *time.Time {
	// 1. Header HTTP padrão.
	if ra := header.Get("Retry-After"); ra != "" {
		// Pode ser delta-seconds ou HTTP-date. Tentamos segundos primeiro.
		if n, err := strconv.Atoi(ra); err == nil && n > 0 && n < 86400*7 {
			t := time.Now().Add(time.Duration(n) * time.Second)
			return &t
		}
		// HTTP-date.
		if t, err := http.ParseTime(ra); err == nil {
			return &t
		}
	}
	// 2. Regex inline no body (provider colocou em JSON em vez de header).
	if m := reRetryAfterInline.FindStringSubmatch(string(body)); len(m) >= 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n < 86400*7 {
			t := time.Now().Add(time.Duration(n) * time.Second)
			return &t
		}
	}
	return nil
}

// sanitizeHint extrai um trecho curto (≤200 chars) e sanitizado do body do
// provedor para fins de UI/auditoria. Delega para o helper ÚNICO compartilhado
// do routing (issue #19) — mesmo comportamento em todos os caminhos.
func sanitizeHint(body []byte) string {
	return routing.SanitizeUpstreamMessage(body)
}

// formatProviderMessage constroi a mensagem de erro exibida na UI para casos
// onde o status NÃO foi persistido (5xx, 4xx não mapeados).
func formatProviderMessage(statusCode int, latency time.Duration, hint string) string {
	msg := fmt.Sprintf("HTTP %d · %dms", statusCode, latency.Milliseconds())
	if statusCode >= 500 {
		msg = "erro do provedor (HTTP " + strconv.Itoa(statusCode) + ") · " + strconv.FormatInt(latency.Milliseconds(), 10) + "ms"
	}
	if hint != "" {
		msg += " · " + hint
	}
	return msg
}

// formatDisabledMessage constroi a mensagem exibida quando a chave foi
// desativada (401/403/429).
func formatDisabledMessage(statusCode int, reason domain.KeyDisabledReason, disabledUntil *time.Time, latency time.Duration) string {
	var head string
	switch statusCode {
	case http.StatusUnauthorized:
		head = "Auth inválida (401)"
	case http.StatusForbidden:
		head = "Auth proibida (403)"
	case http.StatusTooManyRequests:
		head = "Sem cota / rate limit (429)"
	default:
		head = "HTTP " + strconv.Itoa(statusCode)
	}
	if disabledUntil != nil {
		head += " · reativação em " + disabledUntil.Format("02/01 15:04")
	}
	head += " · " + strconv.FormatInt(latency.Milliseconds(), 10) + "ms"
	if reason != "" {
		head += " [" + string(reason) + "]"
	}
	return head
}

// wipeBytes sobrescreve o slice com zeros (defesa em profundidade para o
// plaintext da chave). Best-effort; o GC eventualmente liberaria.
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
