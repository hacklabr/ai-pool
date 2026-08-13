package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestRenderUpdateModelsScript: o script interpola apenas POOL_URL e contém
// os passos essenciais (jq, backup, endpoint ?format=opencode, reescrita
// cirúrgica da chave models).
func TestRenderUpdateModelsScript(t *testing.T) {
	script, err := renderUpdateModelsScript(updateModelsScriptData{
		PoolURL: "https://pool.example.com",
	})
	if err != nil {
		t.Fatalf("renderUpdateModelsScript: %v", err)
	}
	s := string(script)

	for _, want := range []string{
		"set -euo pipefail",
		"'https://pool.example.com'",
		"OPENCODE_CONFIG",
		".config/opencode/opencode.json",
		`provider["llm-pool"].options.apiKey`,
		`provider["llm-pool"].options.baseURL`,
		".bak-",
		"/models?format=opencode",
		`provider["llm-pool"].models = $models`,
		"command -v jq",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

// TestRenderUpdateModelsScript_BashSyntax: o script renderizado passa em
// `bash -n` (syntax check). Pulado se bash não estiver disponível.
func TestRenderUpdateModelsScript_BashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash não disponível; pulando syntax check")
	}
	// POOL_URL com caractere que exigiria escaping correto do shellQuote.
	script, err := renderUpdateModelsScript(updateModelsScriptData{
		PoolURL: "https://pool.example.com/with'quote",
	})
	if err != nil {
		t.Fatalf("renderUpdateModelsScript: %v", err)
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = bytes.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

// TestUpdateModelsScriptHandler: rota pública responde 200 com content-type
// de shellscript, sem exigir autenticação.
func TestUpdateModelsScriptHandler(t *testing.T) {
	a := NewApp(nil, &mockSessionStore{}, nil, true, "https://pool.example.com")
	h := a.Handler()

	req := httptest.NewRequest(http.MethodGet, "/setup/update-models.sh", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/x-shellscript; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/x-shellscript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'https://pool.example.com'") {
		t.Error("script should contain the pool URL (shell-quoted)")
	}
}
