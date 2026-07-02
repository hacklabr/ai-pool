package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// =============================================================================
// Helpers de transporte HTTP da admin API.
//
// Cookies, JSON I/O, context keys, geração de tokens aleatórios. Mantidos
// neste pacote (e não em domain) porque são detalhes de transporte.
// =============================================================================

// ctxKey é tipo privado para chaves de contexto (evita colisão entre pacotes).
type ctxKey int

const (
	ctxKeySession ctxKey = iota
	ctxKeyUser
)

// sessionFromContext retorna a sessão autenticada (ou nil se rota pública).
func sessionFromContext(ctx context.Context) *sessionContext {
	v, _ := ctx.Value(ctxKeySession).(*sessionContext)
	return v
}

// userFromContext retorna o usuário autenticado (populado pela sessionAuth).
func userFromContext(ctx context.Context) *userContext {
	v, _ := ctx.Value(ctxKeyUser).(*userContext)
	return v
}

// sessionContext carrega a sessão + dados derivados populados pela middleware.
type sessionContext struct {
	SessionID string
	CSRFToken string
	UserID    int64
	OrgID     int64
	Username  string
	Role      string
}

// userContext é alias para uniformidade (em rotas /me/*, sessão == usuário).
type userContext = sessionContext

// === Cookies ===

// setSessionCookie escreve o cookie de sessão no response.
// Flags: HttpOnly (sem acesso JS), Secure (apenas HTTPS em prod),
// SameSite=Lax, Path=/. MaxAge alinhado ao TTL do SessionStore.
func (a *App) setSessionCookie(w http.ResponseWriter, sessID string) {
	// #nosec G124 -- Secure/HttpOnly/SameSite são injetados via a.cookieCfg
	// (configurado em buildCookieConfig a partir do modo dev/prod). gosec só
	// detecta literais estáticos, não consegue ver a atribuição via struct.
	c := &http.Cookie{
		Name:     a.cookieCfg.SessionName,
		Value:    sessID,
		Path:     a.cookieCfg.Path,
		Domain:   a.cookieCfg.Domain,
		MaxAge:   a.cookieCfg.MaxAge,
		Secure:   a.cookieCfg.Secure,
		HttpOnly: true,
		SameSite: a.cookieCfg.SameSite,
	}
	http.SetCookie(w, c)
}

// clearSessionCookie invalida o cookie setando MaxAge=-1 + valor vazio.
func (a *App) clearSessionCookie(w http.ResponseWriter) {
	// #nosec G124 -- idem setSessionCookie: flags vindas de a.cookieCfg.
	c := &http.Cookie{
		Name:     a.cookieCfg.SessionName,
		Value:    "",
		Path:     a.cookieCfg.Path,
		Domain:   a.cookieCfg.Domain,
		MaxAge:   -1,
		Secure:   a.cookieCfg.Secure,
		HttpOnly: true,
		SameSite: a.cookieCfg.SameSite,
	}
	http.SetCookie(w, c)
}

// setCSRFCookie escreve o cookie CSRF (NÃO-HttpOnly — JS precisa ler para
// incluir no header X-CSRF-Token em mutações). Double-submit pattern.
func (a *App) setCSRFCookie(w http.ResponseWriter, token string) {
	// #nosec G124 -- Secure/SameSite via a.cookieCfg. HttpOnly=false é por
	// design (double-submit CSRF pattern exige que JS leia o token).
	c := &http.Cookie{
		Name:     a.cookieCfg.CSRFName,
		Value:    token,
		Path:     a.cookieCfg.Path,
		Domain:   a.cookieCfg.Domain,
		MaxAge:   a.cookieCfg.MaxAge,
		Secure:   a.cookieCfg.Secure,
		HttpOnly: false, // JS lê este cookie.
		SameSite: a.cookieCfg.SameSite,
	}
	http.SetCookie(w, c)
}

// clearCSRFCookie invalida o cookie CSRF.
func (a *App) clearCSRFCookie(w http.ResponseWriter) {
	// #nosec G124 -- idem setCSRFCookie.
	c := &http.Cookie{
		Name:     a.cookieCfg.CSRFName,
		Value:    "",
		Path:     a.cookieCfg.Path,
		Domain:   a.cookieCfg.Domain,
		MaxAge:   -1,
		Secure:   a.cookieCfg.Secure,
		HttpOnly: false,
		SameSite: a.cookieCfg.SameSite,
	}
	http.SetCookie(w, c)
}

// readCookie busca um cookie pelo nome. Retorna ("", err) se ausente.
func readCookie(r *http.Request, name string) (string, error) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", err
	}
	return c.Value, nil
}

// === JSON I/O ===

// writeJSON serializa v como JSON e escreve com status dado.
// Define Content-Type application/json e Cache-Control no-store.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Já escrevemos status; logamos (sem slog aqui para manter pacote leve).
		_, _ = w.Write([]byte(`{"error":"internal: json encode failed"}`))
	}
}

// apiErrorEnvelope é o formato padrão de erro da API admin.
type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// writeAPIError escreve um erro no formato envelope.
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorEnvelope{Error: apiErrorBody{Message: message, Code: code}})
}

// parseJSON decodifica o body em v. Retorna erro se body vazio/inválido ou
// excede maxBytes.
// parseJSON decodifica o corpo da requisição no struct v.
//
// Aceita DOIS content-types (a UI é server-rendered com HTMX):
//   - application/json: path estrito (DisallowUnknownFields), usado pelo login
//     (Alpine fetch) e por qualquer cliente JSON.
//   - application/x-www-form-urlencoded (ou multipart): submissões HTMX de
//     forms admin. Decodificado via decodeForm, que faz coerção de tipos
//     (números → int64, "true"/"false" → bool) para campos como provider_id.
func parseJSON(r *http.Request, v any, maxBytes int64) error {
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/x-www-form-urlencoded") || strings.Contains(ct, "multipart/form-data") {
		return decodeForm(r, v)
	}
	if r.Body == nil {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	dec.DisallowUnknownFields() // strict: rejeita campos desconhecidos (defesa mass assignment)
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Rejeita trailing data (JSON com múltiplos objetos, etc.).
	if dec.More() {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

// decodeForm decodifica application/x-www-form-urlencoded (submissões HTMX) no
// struct v via conversão form → JSON → struct. Faz coerção de tipos para que
// campos int64 (ex.: provider_id) e bool funcionem com valores vindos de forms
// HTML (sempre strings).
func decodeForm(r *http.Request, v any) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	obj := make(map[string]any, len(r.Form))
	for k, vals := range r.Form {
		if len(vals) == 0 {
			continue
		}
		raw := vals[0]
		// Campos vazios são pulados: permitem que campos opcionais (ex.:
		// context_window *int, user_agent_override *string) fiquem nil/zero
		// em vez de falhar a coerção (string "" → *int quebraria).
		if raw == "" {
			continue
		}
		switch {
		case raw == "true" || raw == "false":
			obj[k] = raw == "true"
		default:
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				obj[k] = n
			} else {
				obj[k] = raw
			}
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	// Não-strict: forms podem conter campos extras (defensive, sem rejeitar).
	return json.Unmarshal(b, v)
}

// === Tokens aleatórios ===

// randomTokenString gera um token aleatório de nBytes, codificado em
// base64.RawURLEncoding (URL-safe, sem padding). Usado para sessões (32 bytes)
// e CSRF (32 bytes).
//
// Usa crypto/rand (NUNCA math/rand).
func randomTokenString(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// === Helpers de parsing ===

// pathID extrai o {id} de paths como /api/web/me/tokens/123.
// Retorna (0, false) se inválido.
func pathID(path, prefix string) (int64, bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimPrefix(rest, "/")
	// Pega até a próxima barra (se houver subpath).
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// clientIP extrai o IP do cliente do request.
//
// Prioriza X-Forwarded-For (atrás de Ingress/proxy); fallback para RemoteAddr.
// Apenas o PRIMEIRO IP da lista é considerado (original do cliente).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For: client, proxy1, proxy2
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if rri := r.Header.Get("X-Real-IP"); rri != "" {
		return strings.TrimSpace(rri)
	}
	// RemoteAddr é "host:port"; extrai apenas o host.
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx > 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}
