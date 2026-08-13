package admin

import (
	"bytes"
	"fmt"
	"net/http"
	texttemplate "text/template"
)

// =============================================================================
// GET /setup/update-models.sh — script público de atualização do catálogo no
// cliente OpenCode (RF-13).
//
// O script (curl | bash):
//   1. Lê ~/.config/opencode/opencode.json (ou $OPENCODE_CONFIG).
//   2. Extrai provider["llm-pool"].options.apiKey/baseURL (ausentes → mensagem
//      amigável instruindo rodar o instalador; nunca ecoa o token).
//   3. Faz backup do arquivo (opencode.json.bak-<timestamp>).
//   4. Chama GET {baseURL}/models?format=opencode (RF-12) com o token lido.
//   5. Reescreve APENAS provider["llm-pool"].models via jq, preservando todo
//      o restante da configuração.
//
// Rota pública por construção: o script não carrega nenhum segredo — o token
// sai do arquivo local do próprio usuário. O único valor interpolado é
// POOL_URL (AIPP_PUBLIC_URL, com fallback scheme+Host), escapado com
// shellQuote (mesmo padrão do invite).
// =============================================================================

// handleUpdateModelsScript serve o script bash de atualização de modelos.
func (a *App) handleUpdateModelsScript(w http.ResponseWriter, r *http.Request) {
	script, err := renderUpdateModelsScript(updateModelsScriptData{
		PoolURL: a.publicInviteURL(r),
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

// updateModelsScriptData é o contexto para renderização do script bash.
type updateModelsScriptData struct {
	PoolURL string
}

// renderUpdateModelsScript gera o script bash de atualização.
// text/template + shellQuote (mesmo padrão de renderInviteScript).
func renderUpdateModelsScript(data updateModelsScriptData) ([]byte, error) {
	const tpl = `#!/usr/bin/env bash
set -euo pipefail

# -----------------------------------------------------------------------------
# AI Provider Pool — atualiza a lista de modelos do provider 'llm-pool' no
# OpenCode, sem editar arquivo e sem digitar token.
# -----------------------------------------------------------------------------

POOL_URL={{shellQuote .PoolURL}}
CONFIG_FILE="${OPENCODE_CONFIG:-${HOME}/.config/opencode/opencode.json}"

if ! command -v jq >/dev/null 2>&1; then
  echo "Erro: jq é necessário para este script. Instale jq e tente novamente." >&2
  exit 1
fi

if [ ! -f "$CONFIG_FILE" ]; then
  echo "Arquivo de configuração não encontrado: $CONFIG_FILE" >&2
  echo "Rode o instalador do pool primeiro (curl -fsSL $POOL_URL/invite/<token> | bash)." >&2
  exit 1
fi

API_KEY=$(jq -r '.provider["llm-pool"].options.apiKey // empty' "$CONFIG_FILE")
BASE_URL=$(jq -r '.provider["llm-pool"].options.baseURL // empty' "$CONFIG_FILE")

if [ -z "$API_KEY" ] || [ -z "$BASE_URL" ]; then
  echo "Provider 'llm-pool' não configurado em $CONFIG_FILE (apiKey/baseURL ausentes)." >&2
  echo "Rode o instalador do pool primeiro (curl -fsSL $POOL_URL/invite/<token> | bash)." >&2
  exit 1
fi

BACKUP_FILE="${CONFIG_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
cp "$CONFIG_FILE" "$BACKUP_FILE"

echo "Consultando o catálogo de modelos do pool..."
MODELS=$(curl -fsSL -H "Authorization: Bearer $API_KEY" "${BASE_URL}/models?format=opencode")

jq --argjson models "$MODELS" \
   '.provider["llm-pool"].models = $models' \
   "$CONFIG_FILE" > "${CONFIG_FILE}.tmp" && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"

echo "Modelos atualizados com sucesso em $CONFIG_FILE"
echo "Backup salvo em $BACKUP_FILE"
`
	tmpl, err := texttemplate.New("update-models").Funcs(texttemplate.FuncMap{
		"shellQuote": shellQuote,
	}).Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("parse update-models script template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute update-models script template: %w", err)
	}
	return buf.Bytes(), nil
}
