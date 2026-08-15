package modelsdev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCatalogOfficialWins(t *testing.T) {
	body := []byte(`{
	  "zai": {"models": {
	    "glm-5.3": {"limit": {"context": 1000000, "output": 131072},
	                "modalities": {"input": ["text"], "output": ["text"]},
	                "reasoning_options": [{"type": "effort", "values": ["high", "max"]}]}
	  }},
	  "openrouter": {"models": {
	    "glm-5.3": {"limit": {"context": 999, "output": 999}}
	  }},
	  "kimi-for-coding": {"models": {
	    "k3": {"limit": {"context": 1048576, "output": 131072},
	           "modalities": {"input": ["text", "image", "video"], "output": ["text"]},
	           "reasoning_options": [{"type": "toggle"}, {"type": "effort", "values": ["low", "high", "max"]}]}
	  }}
	}`)
	idx, err := parseCatalog(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if idx.Len() != 2 {
		t.Fatalf("len = %d, want 2", idx.Len())
	}
	spec, ok := idx.Lookup("glm-5.3")
	if !ok {
		t.Fatal("glm-5.3 not found")
	}
	if spec.ContextWindow != 1000000 || spec.OutputLimit != 131072 {
		t.Errorf("limits = %d/%d, want oficial 1000000/131072 (não 999 do agregador)",
			spec.ContextWindow, spec.OutputLimit)
	}
	if len(spec.ReasoningEffort) != 2 || spec.ReasoningEffort[0] != "high" || spec.ReasoningEffort[1] != "max" {
		t.Errorf("efforts = %v, want [high max]", spec.ReasoningEffort)
	}

	k3, ok := idx.Lookup("k3")
	if !ok {
		t.Fatal("k3 not found")
	}
	wantMods := []string{"text", "image", "video"}
	if len(k3.Modalities) != len(wantMods) {
		t.Fatalf("modalities = %v, want %v", k3.Modalities, wantMods)
	}
	for i, m := range wantMods {
		if k3.Modalities[i] != m {
			t.Fatalf("modalities = %v, want %v", k3.Modalities, wantMods)
		}
	}
	if len(k3.ReasoningEffort) != 3 {
		t.Errorf("k3 efforts = %v, want 3 valores", k3.ReasoningEffort)
	}
}

func TestParseSkipsIncomplete(t *testing.T) {
	body := []byte(`{
	  "x": {"models": {
	    "": {"limit": {"context": 1, "output": 1}},
	    "sem-limit": {"name": "Sem limit"},
	    "so-context": {"limit": {"context": 100}},
	    "ok": {"limit": {"context": 10, "output": 5}, "modality": {"input": ["text"], "output": ["text"]}}
	  }}
	}`)
	idx, err := parseCatalog(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := idx.Lookup("sem-limit"); ok {
		t.Error("sem-limit deveria ser ignorado (sem limit)")
	}
	if _, ok := idx.Lookup("so-context"); ok {
		t.Error("so-context deveria ser ignorado (sem output)")
	}
	spec, ok := idx.Lookup("ok")
	if !ok {
		t.Fatal("ok not found")
	}
	if spec.ContextWindow != 10 || spec.OutputLimit != 5 {
		t.Errorf("limits = %d/%d, want 10/5", spec.ContextWindow, spec.OutputLimit)
	}
	if len(spec.Modalities) != 1 || spec.Modalities[0] != "text" {
		t.Errorf("modalities legado = %v, want [text]", spec.Modalities)
	}
}

func TestLookupCopyIsDefensive(t *testing.T) {
	idx, err := parseCatalog([]byte(`{"zai":{"models":{"glm-5.3":{"limit":{"context":1,"output":1},
		"reasoning_options":[{"type":"effort","values":["high","max"]}]}}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	spec, _ := idx.Lookup("glm-5.3")
	spec.ReasoningEffort[0] = "mutado"
	again, _ := idx.Lookup("glm-5.3")
	if again.ReasoningEffort[0] != "high" {
		t.Error("Lookup deve retornar cópia defensiva")
	}
}

func TestFetchFailSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(nil)
	c.url = srv.URL
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("500 deveria retornar erro (fail-safe)")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"zai":{"models":{"glm-5.3":{"limit":{"context":1,"output":1}}}}}`))
	}))
	defer srv2.Close()
	c2 := NewClient(nil)
	c2.url = srv2.URL
	idx, err := c2.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch ok: %v", err)
	}
	if idx.Len() != 1 {
		t.Errorf("len = %d, want 1", idx.Len())
	}
}
