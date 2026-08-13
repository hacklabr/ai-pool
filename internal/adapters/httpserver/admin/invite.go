package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	texttemplate "text/template"

	"github.com/hacklabr/ai-pool/internal/adapters/httpserver/modelcatalog"
	"github.com/hacklabr/ai-pool/internal/adapters/postgres"
	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Endpoints públicos de convite (sem autenticação).
//
//   - GET  /invite/{token}                  → script bash de setup
//   - POST /api/web/invite/{token}/accept   → aceita convite, cria usuário + token
//
// Segurança:
//   - O token de convite é validado por hash SHA-256 em tempo constante.
//   - GET /invite/{token} retorna o MESMO script independentemente de o token
//     ser válido, usado, expirado ou inexistente — impede enumeration.
//   - POST /api/invite/{token}/accept é rate-limitado por IP.
//   - Aceite é feito em transação com SELECT ... FOR UPDATE.
// =============================================================================

// handleInviteScript retorna o script bash de setup para qualquer token.
//
// O script é idêntico para token válido/inválido/usado/expirado. A validação
// real acontece apenas no POST /api/invite/{token}/accept. Isto previne
// enumeration de tokens válidos.
func (a *App) handleInviteScript(w http.ResponseWriter, r *http.Request) {
	// Extrai token do path /invite/{token}
	token := strings.TrimPrefix(r.URL.Path, "/invite/")
	token = strings.TrimPrefix(token, "/")
	if token == "" {
		// Não revelamos ausência de token; retornamos script genérico.
		token = "invalid-token"
	}

	// Validamos o formato apenas para evitar injeção no template; o script em si
	// não executa nada com o token além de passá-lo para o POST.
	if len(token) > 128 {
		token = token[:128]
	}

	publicURL := a.publicInviteURL(r)
	script, err := renderInviteScript(inviteScriptData{
		Token:     token,
		AcceptURL: publicURL + "/api/web/invite/" + token + "/accept",
		PoolURL:   publicURL + "/api",
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(script)
}

// handleInviteAccept aceita um convite válido e cria o usuário + token API.
//
// Fluxo:
//  1. Rate-limit por IP.
//  2. Valida formato do token.
//  3. Transação: SELECT ... FOR UPDATE do convite pending.
//  4. Verifica expiração.
//  5. Cria usuário com senha Argon2id.
//  6. Gera token API sk-aipp-... com name="opencode".
//  7. Marca convite como used.
//  8. Commit.
//  9. Retorna dados para configuração do OpenCode (se solicitado).
func (a *App) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	// Rate-limit: 5 tentativas / 15 min por IP.
	if a.rateLimiter != nil {
		allowed, retryAfter, err := a.rateLimiter.Allow(r.Context(), "invite_accept_ip:"+ip)
		if err == nil && !allowed {
			retrySec := int(retryAfter.Seconds())
			if retrySec < 1 {
				retrySec = 1
			}
			writeRateLimited(w, "too many attempts; please try again later", retrySec)
			return
		}
	}

	// Extrai token do path /api/web/invite/{token}/accept
	token := extractInviteToken(r.URL.Path)
	if token == "" {
		writeInviteError(w, http.StatusBadRequest, "invalid invitation")
		return
	}

	tokenHash := hashRawInviteToken(token)

	var req acceptInvitationRequest
	if err := parseJSON(r, &req, 4*1024); err != nil {
		writeInviteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	result, err := a.invitationRepo.Accept(r.Context(), tokenHash, req.Password, a.pepper)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvitationWeakPassword):
			writeInviteError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, postgres.ErrNotFound), errors.Is(err, postgres.ErrConflict):
			writeInviteError(w, http.StatusBadRequest, "invalid or expired invitation")
		default:
			writeInviteError(w, http.StatusInternalServerError, "could not accept invitation")
		}
		return
	}

	// Carrega modelos ativos para retorno.
	models, err := a.activeModels(r.Context(), result.OrgID)
	if err != nil {
		models = map[string]any{}
	}

	// Auditoria.
	usedBy := int64(result.UserID)
	_ = a.auditRepo.Append(r.Context(), postgres.AuditEntry{
		OrganizationID: int64(result.OrgID),
		ActorUserID:    &usedBy,
		Action:         "invitation.accept",
		EntityType:     "invitation_token",
		Metadata: map[string]any{
			"username":       result.Username,
			"token_prefix":   result.TokenPrefix,
			"setup_opencode": req.SetupOpencode,
			"setup_mimocode": req.SetupMimocode,
		},
		IP:        net.ParseIP(ip),
		UserAgent: r.UserAgent(),
	})

	writeJSON(w, http.StatusOK, acceptInvitationResponse{
		User: userPublic{
			ID:       int64(result.UserID),
			Username: result.Username,
			Role:     string(domain.RoleUser),
			OrgID:    int64(result.OrgID),
		},
		APIToken: result.APITokenRaw,
		Models:   models,
	})
}

// extractInviteToken extrai o token de /api/web/invite/{token}/accept.
func extractInviteToken(path string) string {
	prefix := "/api/web/invite/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimPrefix(rest, "/")
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	if rest == "" {
		return ""
	}
	return rest
}

// hashRawInviteToken calcula SHA-256 hex de um token raw.
func hashRawInviteToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// writeInviteError retorna erro uniforme para não revelar detalhes do token.
func writeInviteError(w http.ResponseWriter, status int, message string) {
	writeAPIError(w, status, "invalid_invitation", message)
}

// writeRateLimited responde 429 com Retry-After.
func writeRateLimited(w http.ResponseWriter, message string, retrySec int) {
	w.Header().Set("Retry-After", strconv.Itoa(retrySec))
	writeAPIError(w, http.StatusTooManyRequests, "rate_limited", message)
}

// activeModels retorna os modelos ativos da organização no formato do OpenCode.
// A conversão (modalities + bloco "limit") vive no pacote modelcatalog,
// compartilhado com o endpoint agregado GET /v1/models?format=opencode (RF-12).
func (a *App) activeModels(ctx context.Context, orgID domain.OrganizationID) (map[string]any, error) {
	all, err := a.modelRepo.ListAll(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return modelcatalog.OpenCodeModelsMap(all), nil
}

// inviteScriptData é o contexto para renderização do script bash.
type inviteScriptData struct {
	Token     string
	AcceptURL string
	PoolURL   string
}

// renderInviteScript gera o script bash de setup.
//
// Usa text/template para controle total do escaping. Todos os valores
// interpolados são escapados para JavaScript/string JSON (pois o script faz
// fetch com JSON) e para shell single-quote.
func renderInviteScript(data inviteScriptData) ([]byte, error) {
	const tpl = `#!/usr/bin/env bash
set -euo pipefail

# -----------------------------------------------------------------------------
# Script de setup do AI Provider Pool
# Este link é pessoal e de uso único. Não compartilhe.
# -----------------------------------------------------------------------------

TOKEN={{shellQuote .Token}}
ACCEPT_URL={{shellQuote .AcceptURL}}
POOL_URL={{shellQuote .PoolURL}}

echo "Bem-vindo ao AI Provider Pool!"
echo ""

# Solicita senha
# < /dev/tty é necessário porque curl|bash conecta stdin ao pipe do curl,
# não ao terminal. Sem isso, read lê do pipe (vazio) em vez do teclado.
while true; do
  read -rsp "Defina uma senha (mínimo 12 caracteres): " PASSWORD < /dev/tty
  echo
  if [ "${#PASSWORD}" -ge 12 ]; then
    break
  fi
  echo "Senha muito curta. Tente novamente."
done

read -rsp "Confirme a senha: " PASSWORD2 < /dev/tty
echo
if [ "$PASSWORD" != "$PASSWORD2" ]; then
  echo "Senhas não conferem. Abortando."
  exit 1
fi

# Verifica dependências
if ! command -v jq >/dev/null 2>&1; then
  echo "Erro: jq é necessário para este script. Instale jq e tente novamente."
  exit 1
fi

# Pergunta sobre OpenCode
read -rp "Configurar OpenCode automaticamente? [s/N]: " SETUP_OPENCODE < /dev/tty
SETUP_OPENCODE=${SETUP_OPENCODE:-N}
if [[ "$SETUP_OPENCODE" =~ ^[Ss]$ ]]; then
  DO_OPENCODE='true'
else
  DO_OPENCODE='false'
fi

# Pergunta sobre MiMo-Code
read -rp "Configurar MiMo-Code automaticamente? [s/N]: " SETUP_MIMOCODE < /dev/tty
SETUP_MIMOCODE=${SETUP_MIMOCODE:-N}
if [[ "$SETUP_MIMOCODE" =~ ^[Ss]$ ]]; then
  DO_MIMOCODE='true'
else
  DO_MIMOCODE='false'
fi

echo ""
echo "Criando usuário e token de API..."

RESPONSE=$(curl -fsSL -X POST "$ACCEPT_URL" \
  -H "Content-Type: application/json" \
  -d "{\"password\":$(printf '%s' "$PASSWORD" | jq -Rs .),\"setup_opencode\":$DO_OPENCODE,\"setup_mimocode\":$DO_MIMOCODE}")

API_TOKEN=$(echo "$RESPONSE" | jq -r '.api_token')
USERNAME=$(echo "$RESPONSE" | jq -r '.user.username')

if [ "$API_TOKEN" = "null" ] || [ -z "$API_TOKEN" ]; then
  echo "Erro ao criar conta. Resposta:"
  echo "$RESPONSE"
  exit 1
fi

echo "Usuário '$USERNAME' criado com sucesso."

# -----------------------------------------------------------------------------
# Função: configura o provider 'llm-pool' em um arquivo de config JSON.
# Cria o arquivo se não existir; sobrescreve o provider se já existir.
# -----------------------------------------------------------------------------
configure_provider() {
  local config_file="$1"
  local config_dir
  config_dir=$(dirname "$config_file")
  mkdir -p "$config_dir"

  local models
  models=$(echo "$RESPONSE" | jq '.models // {}')

  if [ -f "$config_file" ]; then
    echo "Atualizando $config_file"
    jq --arg provider_name "llm-pool" \
       --arg base_url "$POOL_URL" \
       --arg api_key "$API_TOKEN" \
       --argjson models "$models" \
       '.provider[$provider_name] = {
          "npm": "@ai-sdk/openai-compatible",
          "name": $provider_name,
          "options": { "baseURL": $base_url, "apiKey": $api_key },
          "models": $models
        }' "$config_file" > "$config_file.tmp" && mv "$config_file.tmp" "$config_file"
  else
    echo "Criando $config_file"
    jq -n --arg provider_name "llm-pool" \
          --arg base_url "$POOL_URL" \
          --arg api_key "$API_TOKEN" \
          --argjson models "$models" \
          '{provider: {($provider_name): {
             "npm": "@ai-sdk/openai-compatible",
             "name": $provider_name,
             "options": { "baseURL": $base_url, "apiKey": $api_key },
             "models": $models
           }}}' > "$config_file"
  fi
}

if [ "$DO_OPENCODE" = 'true' ]; then
  configure_provider "${HOME}/.config/opencode/opencode.json"
  echo "OpenCode configurado com o provider 'llm-pool'."
fi

if [ "$DO_MIMOCODE" = 'true' ]; then
  configure_provider "${HOME}/.config/mimocode/mimocode.json"
  echo "MiMo-Code configurado com o provider 'llm-pool'."
fi

echo ""
echo "Seu token de API: ${API_TOKEN:0:20}..."
echo "Guarde-o em um local seguro. Ele não será exibido novamente."
`
	tmpl, err := texttemplate.New("invite").Funcs(texttemplate.FuncMap{
		"shellQuote": shellQuote,
	}).Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("parse invite script template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute invite script template: %w", err)
	}
	return buf.Bytes(), nil
}

// shellQuote escapa uma string para uso em shell com single quotes.
func shellQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}
