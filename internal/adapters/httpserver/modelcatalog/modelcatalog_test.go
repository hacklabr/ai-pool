package modelcatalog

import (
	"testing"

	"github.com/hacklabr/ai-pool/internal/domain"
)

func intPtr(v int) *int { return &v }

func strPtr(v string) *string { return &v }

// TestOpenCodeModelsMap: conversão para o schema OpenCode — filtra inativos,
// usa DisplayName, emite modalities só para multimodal; limit é SEMPRE
// emitido com context+output completos (issue #29: o OpenCode exige
// limit.output; modelos sem spec conhecida recebem defaults conservadores).
func TestOpenCodeModelsMap(t *testing.T) {
	models := []domain.Model{
		{Name: "plain", IsActive: true},
		{Name: "rich", DisplayName: strPtr("Rich Model"), ContextWindow: intPtr(128000),
			OutputLimit: intPtr(4096), Modalities: []string{"text", "image"}, IsActive: true},
		{Name: "ctx-only", ContextWindow: intPtr(32000), IsActive: true},
		{Name: "off", IsActive: false},
	}

	out := OpenCodeModelsMap(models)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3 (inactive excluded)", len(out))
	}
	if _, ok := out["off"]; ok {
		t.Error("inactive model should be excluded")
	}

	plain := out["plain"].(map[string]any)
	if plain["name"] != "plain" {
		t.Errorf("plain name = %v, want 'plain'", plain["name"])
	}
	limitP := plain["limit"].(map[string]any)
	if limitP["context"] != DefaultContextWindow || limitP["output"] != DefaultOutputLimit {
		t.Errorf("plain limit = %v, want defaults {context:%d output:%d}",
			limitP, DefaultContextWindow, DefaultOutputLimit)
	}
	if _, ok := plain["modalities"]; ok {
		t.Error("plain should not have modalities")
	}

	rich := out["rich"].(map[string]any)
	if rich["name"] != "Rich Model" {
		t.Errorf("rich name = %v, want DisplayName", rich["name"])
	}
	limit := rich["limit"].(map[string]any)
	if limit["context"] != 128000 || limit["output"] != 4096 {
		t.Errorf("rich limit = %v, want {context:128000 output:4096}", limit)
	}
	if _, ok := rich["modalities"]; !ok {
		t.Error("rich (multimodal) should have modalities")
	}

	ctxOnly := out["ctx-only"].(map[string]any)
	limit2 := ctxOnly["limit"].(map[string]any)
	if limit2["context"] != 32000 {
		t.Errorf("ctx-only limit.context = %v, want 32000", limit2["context"])
	}
	if limit2["output"] != DefaultOutputLimit {
		t.Errorf("ctx-only limit.output = %v, want default %d (schema validity, issue #29)",
			limit2["output"], DefaultOutputLimit)
	}
}

// TestOpenCodeModelsMap_Empty: entrada vazia → mapa vazio (nunca nil no JSON).
func TestOpenCodeModelsMap_Empty(t *testing.T) {
	out := OpenCodeModelsMap(nil)
	if out == nil || len(out) != 0 {
		t.Errorf("out = %v, want empty non-nil map", out)
	}
}

// TestOpenAIList: formato OpenAI — data é sempre array (nunca null).
func TestOpenAIList(t *testing.T) {
	entries := []Entry{{Name: "m1", ProviderSlug: "deepseek", CreatedUnix: 1700000000}}
	out := OpenAIList(entries)
	if out["object"] != "list" {
		t.Errorf("object = %v, want list", out["object"])
	}
	data := out["data"].([]map[string]any)
	if len(data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(data))
	}
	e := data[0]
	if e["id"] != "m1" || e["object"] != "model" || e["owned_by"] != "deepseek" || e["created"] != int64(1700000000) {
		t.Errorf("entry = %v", e)
	}

	empty := OpenAIList(nil)
	if d := empty["data"].([]map[string]any); d == nil || len(d) != 0 {
		t.Errorf("empty data = %v, want empty non-nil slice", d)
	}
}
