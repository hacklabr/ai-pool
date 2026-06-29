package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Forwarder — monta e envia a requisição ao provedor upstream.
//
// Princípio (spec §3.2.1): REPASSE TRANSPARENTE preservando os BYTES ORIGINAIS
// do body. Decisões de roteamento (model, stream) usam parse PARCIAL do body;
// o que é enviado ao upstream são os bytes exatos que o cliente enviou.
//
// Isto é CRÍTICO para:
//   - Não quebrar cache de contexto do provedor (preserva whitespaces, ordem
//     de keys, campos extras que o cliente pode ter adicionado).
//   - Garantir compatibilidade total com SDKs OpenAI que podem enviar campos
//     que o proxy não conhece ainda.
//
// Fluxo:
//   1. Lê body original em []byte.
//   2. Faz parse parcial (json.Decoder + struct só com {model, stream}) para
//      decisões de roteamento.
//   3. Resolve provider+key+decrypted via KeyResolver.
//   4. Monta http.Request para o provider com:
//        - Method + path preservados.
//        - Body = bytes ORIGINAIS (não re-serializados).
//        - Authorization: Bearer <decrypted_key>.
//        - User-Agent override se provider/key definir.
//        - Demais headers repassados via allowlist (removendo Authorization,
//          Cookie, hop-by-hop, Accept-Encoding).
//   5. Envia com o http.Client injetado, usando o context (com timeouts).
//
// Não há failover aqui — o caller (Server.handleV1) decide se tenta outra
// chave em caso de erro (F5). Em F3, qualquer erro é terminal.
// =============================================================================

// upstreamRequestSpec carrega a decisão de roteamento (parse parcial do body).
type upstreamRequestSpec struct {
	Model    string
	IsStream bool
}

// UpstreamRequestSpec é a versão EXPORTADA de upstreamRequestSpec, usada pelo
// pacote failover (wrapper do chain) que precisa repassar o spec ao proxy.
// Mantemos a forma privada internamente para evitar confusão de APIs.
type UpstreamRequestSpec = upstreamRequestSpec

// parseRequestSpec faz o parse PARCIAL do body OpenAI para extrair o model e
// o flag stream, SEM re-serializar. Retorna o spec + os bytes originais.
//
// Body deve ser JSON válido no formato OpenAI-compatible. Campos desconhecidos
// são ignorados (DisallowUnknownFields=false).
func parseRequestSpec(body []byte) (upstreamRequestSpec, error) {
	var partial struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		// Resto é capturado em RawMessage para validação de parse (não usado).
		Raw json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return upstreamRequestSpec{}, fmt.Errorf("parse body: %w", err)
	}
	return upstreamRequestSpec{
		Model:    partial.Model,
		IsStream: partial.Stream,
	}, nil
}

// hop-by-hop headers que NUNCA devem ser repassados (RFC 7230 §6.1).
// Eles são por-conexão e fazem sentido apenas entre dois hops diretos.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// forwardedClientHeaders é a ALLOWLIST positiva de headers do cliente que
// repassamos ao provedor. Tudo que NÃO está aqui é descartado (default-deny).
//
// Authorization é tratada explicitamente (sempre substituída pela chave).
// Accept-Encoding é descartada para forçar stream de texto puro (simplifica
// debugging e o proxy não descomprime).
var forwardedClientHeaders = map[string]bool{
	"Content-Type": true,
	"Accept":       true,
}

// proxyPathPrefixes são os prefixos que o proxy aceita e DESCARTA antes de
// montar a URL upstream. A versão do provedor (ex.: /v1 do OpenAI, /v4 do
// Z.ai, /v1 do Kimi) já vive no base_url configurado — o proxy só repassa o
// endpoint. Confirmado empiricamente: base+/chat/completions → 200,
// base+/v1/chat/completions → 404 (versão dobrada).
//
// Suporta AMBOS (sem custo extra; atende dois públicos):
//   - /v1/*  → SDKs OpenAI (client base_url = http://proxy/v1)
//   - /api/* → ferramentas de coding (ex.: /api/autocomplete, /api/chat/completions)
//
// /api/web/* (API interna da UI admin) NÃO é do proxy — o composeHandler do
// main.go a roteia ao admin ANTES deste prefixo, evitando colisão.
var proxyPathPrefixes = []string{"/v1", "/api"}

// stripProxyPrefix remove o prefixo do proxy (/v1 ou /api) do path, deixando
// apenas o endpoint a ser anexado ao base_url do provedor.
//
//	"/v1/chat/completions" → "/chat/completions"
//	"/api/autocomplete"    → "/autocomplete"
//	"/chat/completions"    → "/chat/completions"  (sem prefixo, passa direto)
func stripProxyPrefix(p string) string {
	for _, pref := range proxyPathPrefixes {
		if p == pref {
			return ""
		}
		if strings.HasPrefix(p, pref+"/") {
			return strings.TrimPrefix(p, pref)
		}
	}
	return p
}

// ConversationIDFromRequest extrai o identificador de sessão/conversa dos
// headers do cliente, na prioridade:
//  1. X-Session-Id        (enviado pelo OpenCode — estável por sessão)
//  2. X-Session-Affinity  (variante do OpenCode)
//  3. X-Conversation-ID   (convenção genérica)
//
// Usado pela afinidade de cache de contexto: mesmo ID → mesma chave (aproveita
// o cache do provedor, crítico para contextos grandes como os do coding agent).
// Retorna "" se nenhum header estiver presente (cai para LRU/rotação).
func ConversationIDFromRequest(r *http.Request) string {
	for _, h := range []string{"X-Session-Id", "X-Session-Affinity", "X-Conversation-ID"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

// buildUpstreamRequest monta o http.Request para o provedor a partir da
// requisição do cliente. Forma EXPORTADA como BuildUpstreamRequest (alias)
// para uso pelo pacote failover wrapper.
func buildUpstreamRequest(
	clientReq *http.Request,
	bodyBytes []byte,
	resolution *domain.KeyResolution,
	isStream bool,
) (*http.Request, error) {
	// URL final = base_url do provider + endpoint (prefixo do proxy DESCARTADO)
	// + query preservada. A versão do provider já está no base_url.
	forwardPath := stripProxyPrefix(clientReq.URL.Path)
	targetURL := strings.TrimRight(resolution.Provider.BaseURL, "/") + forwardPath
	if clientReq.URL.RawQuery != "" {
		targetURL += "?" + clientReq.URL.RawQuery
	}

	// Para captura de uso em streaming: injeta stream_options.include_usage=true
	// (se o cliente não pediu) p/ que o provedor reporte usage no chunk final.
	// Repassa um body NOVO (não muta bodyBytes — o hash de afinidade já foi
	// computado sobre o body original pelo wrapper).
	body := bodyBytes
	if isStream {
		body = ensureStreamIncludeUsage(bodyBytes)
	}
	upReq, err := http.NewRequestWithContext(clientReq.Context(), clientReq.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	// === Headers ===

	// 1. Allowlist positiva do cliente (Content-Type, Accept).
	for h, vals := range clientReq.Header {
		hc := http.CanonicalHeaderKey(h)
		if forwardedClientHeaders[hc] {
			for _, v := range vals {
				upReq.Header.Add(hc, v)
			}
		}
	}

	// 2. Remoção de hop-by-hop (defesa — alguns proxies clients insistem).
	for _, h := range hopByHopHeaders {
		upReq.Header.Del(h)
	}

	// 3. Accept-Encoding: identity — força texto puro (sem gzip do provider).
	// Isto simplifica streaming/inspeção. O custo é banda, aceitável para
	// chat (texto é altamente comprimível mas não dominante).
	upReq.Header.Set("Accept-Encoding", "identity")

	// 4. Authorization: SEMPRE substituída pela chave do provedor.
	// CRÍTICO: nunca repassar o token sk-aipp-... do cliente ao provedor.
	upReq.Header.Set("Authorization", "Bearer "+string(resolution.DecryptedValue))

	// 5. User-Agent override (provider global ou por chave).
	// Provider.UserAgentGlobal é fallback; Key.UserAgentOverride tem precedência.
	ua := resolution.Provider.UserAgentGlobal
	if resolution.Key.UserAgentOverride != nil && *resolution.Key.UserAgentOverride != "" {
		ua = resolution.Key.UserAgentOverride
	}
	if ua != nil && *ua != "" {
		upReq.Header.Set("User-Agent", *ua)
	}

	// 6. Host — Go ajusta automaticamente baseado na URL alvo. Não precisamos
	// setar explicitamente; alguns provedores validam Host estrito.

	// Content-Length é derivado automaticamente por Go a partir do bytes.Reader.
	return upReq, nil
}

// BuildUpstreamRequest é a forma exportada de buildUpstreamRequest.
// Usada pelo wrapper failover (fora do pacote proxy).
func BuildUpstreamRequest(
	clientReq *http.Request,
	bodyBytes []byte,
	resolution *domain.KeyResolution,
	isStream bool,
) (*http.Request, error) {
	return buildUpstreamRequest(clientReq, bodyBytes, resolution, isStream)
}

// ensureStreamIncludeUsage injeta stream_options.include_usage=true no body JSON
// de uma requisição de streaming, para que o provedor reporte usage no chunk
// final (sem isso, streams chegam com 0 tokens no usage log).
//
// Preserva a escolha do cliente: se já pediu include_usage, não toca. Re-
// serializa o JSON (ordem de keys pode mudar — irrelevante p/ provedores; o
// hash de afinidade é computado sobre o body original, antes desta chamada).
// Se o body não é um objeto JSON válido, retorna o original intacto.
func ensureStreamIncludeUsage(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	if so, ok := m["stream_options"].(map[string]any); ok {
		if v, _ := so["include_usage"].(bool); v {
			return body // cliente já pediu — respeitar
		}
	}
	m["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// sendUpstream envia o request e retorna a response. Caller é responsável por
// fechar resp.Body.
//
// O contexto do request JÁ DEVE ter os timeouts (overall, etc.) aplicados.
// Não adicionamos timeout aqui — quem chama controla o ciclo de vida.
func sendUpstream(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream Do: %w", err)
	}
	return resp, nil
}

// readBodyAndRestore lê o body do request em []byte e devolve um novo request
// com o body restaurado (para que handlers downstream possam reler se precisar).
//
// Limita o tamanho via maxBytes (defesa DoS — 413 se exceder).
//
// Implementação: usa io.LimitReader com limite maxBytes+1 e verifica se lemos
// MAIS que maxBytes (sinal de body grande demais). Isto é mais robusto que
// http.MaxBytesReader (que depende do ResponseWriter para sinalizar 413 e pode
// não funcionar em todos os cenários de middleware chain).
//
// Retorna *http.MaxBytesError quando excede — caller detecta via errors.As.
func readBodyAndRestore(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		// Restaura body mesmo em caso de erro (defensivo).
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	// Restaura o body para eventuais re-leituras (logging, etc.).
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
