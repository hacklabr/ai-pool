package modelregistry

import (
	"strings"
	"testing"
	"testing/fstest"
)

// =============================================================================
// Testes do dicionário embutido (issue #26): lookup por nome exato, alias,
// alias com override, nome desconhecido; validação do JSON no load.
// =============================================================================

// TestDefault_ValidAndLookups valida o specs.json EMBUTIDO: parse OK e
// lookups corretos contra os dados validados (Z.ai/Kimi).
func TestDefault_ValidAndLookups(t *testing.T) {
	r, err := Default()
	if err != nil {
		t.Fatalf("Default() failed: %v", err)
	}

	tests := []struct {
		name          string
		canonical     string
		contextWindow int
		outputLimit   int
		modalities    []string
	}{
		{name: "glm-4.5", canonical: "glm-4.5", contextWindow: 131072, outputLimit: 98304, modalities: []string{"text"}},
		{name: "glm-5.2", canonical: "glm-5.2", contextWindow: 1048576, outputLimit: 131072, modalities: []string{"text"}},
		{name: "kimi-k3", canonical: "kimi-k3", contextWindow: 1048576, outputLimit: 1048576, modalities: []string{"text", "image"}},
		// alias herda tudo do pai, Canonical aponta para o pai.
		{name: "k3", canonical: "kimi-k3", contextWindow: 1048576, outputLimit: 1048576, modalities: []string{"text", "image"}},
		// alias com override de context_window (saída/modalidades herdam).
		{name: "k3-256k", canonical: "kimi-k3", contextWindow: 262144, outputLimit: 1048576, modalities: []string{"text", "image"}},
		// alias herda tudo.
		{name: "kimi-for-coding", canonical: "kimi-k2.7-code", contextWindow: 262144, outputLimit: 32768, modalities: []string{"text", "image"}},
		{name: "kimi-for-coding-highspeed", canonical: "kimi-k2.7-code", contextWindow: 262144, outputLimit: 32768, modalities: []string{"text", "image"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := r.Lookup(tc.name)
			if !ok {
				t.Fatalf("Lookup(%q) not found, want found", tc.name)
			}
			if spec.Canonical != tc.canonical {
				t.Errorf("Canonical = %q, want %q", spec.Canonical, tc.canonical)
			}
			if spec.ContextWindow != tc.contextWindow {
				t.Errorf("ContextWindow = %d, want %d", spec.ContextWindow, tc.contextWindow)
			}
			if spec.OutputLimit != tc.outputLimit {
				t.Errorf("OutputLimit = %d, want %d", spec.OutputLimit, tc.outputLimit)
			}
			if len(spec.Modalities) != len(tc.modalities) {
				t.Fatalf("Modalities = %v, want %v", spec.Modalities, tc.modalities)
			}
			for i := range tc.modalities {
				if spec.Modalities[i] != tc.modalities[i] {
					t.Errorf("Modalities[%d] = %q, want %q", i, spec.Modalities[i], tc.modalities[i])
				}
			}
		})
	}
}

// TestLookup_Unknown: nome fora do dicionário (e variações de caso) não bate.
func TestLookup_Unknown(t *testing.T) {
	r, err := Default()
	if err != nil {
		t.Fatalf("Default() failed: %v", err)
	}
	for _, name := range []string{"deepseek-chat", "", "GLM-4.5", "Kimi-K3", "kimi-k4"} {
		if _, ok := r.Lookup(name); ok {
			t.Errorf("Lookup(%q) found, want not found", name)
		}
	}
}

// TestLookup_ModalitiesDefensiveCopy: mutar o slice retornado não altera o
// dicionário (dados imutáveis em memória).
func TestLookup_ModalitiesDefensiveCopy(t *testing.T) {
	r, err := Default()
	if err != nil {
		t.Fatalf("Default() failed: %v", err)
	}
	spec, _ := r.Lookup("kimi-k3")
	spec.Modalities[0] = "tampered"
	again, _ := r.Lookup("kimi-k3")
	if again.Modalities[0] != "text" {
		t.Errorf("mutating returned slice leaked into registry: %v", again.Modalities)
	}
}

func mapFSWithJSON(t *testing.T, json string) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{"specs.json": &fstest.MapFile{Data: []byte(json)}}
}

// TestLoad_Validation: conteúdo inválido falha rápido no load.
func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "invalid JSON", json: `not json`},
		{name: "empty canonical", json: `{"models":[{"canonical":"","context_window":1,"output_limit":1,"modalities":["text"]}]}`},
		{name: "non-positive context_window", json: `{"models":[{"canonical":"m","context_window":0,"output_limit":1,"modalities":["text"]}]}`},
		{name: "non-positive output_limit", json: `{"models":[{"canonical":"m","context_window":1,"output_limit":-1,"modalities":["text"]}]}`},
		{name: "empty modalities", json: `{"models":[{"canonical":"m","context_window":1,"output_limit":1}]}`},
		{name: "duplicate canonical", json: `{"models":[` +
			`{"canonical":"m","context_window":1,"output_limit":1,"modalities":["text"]},` +
			`{"canonical":"m","context_window":2,"output_limit":2,"modalities":["text"]}]}`},
		{name: "alias collides with canonical", json: `{"models":[` +
			`{"canonical":"m","context_window":1,"output_limit":1,"modalities":["text"],"aliases":[{"name":"x"}]},` +
			`{"canonical":"x","context_window":2,"output_limit":2,"modalities":["text"]}]}`},
		{name: "alias empty name", json: `{"models":[` +
			`{"canonical":"m","context_window":1,"output_limit":1,"modalities":["text"],"aliases":[{"name":""}]}]}`},
		{name: "alias non-positive override", json: `{"models":[` +
			`{"canonical":"m","context_window":1,"output_limit":1,"modalities":["text"],"aliases":[{"name":"a","context_window":0}]}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(mapFSWithJSON(t, tc.json), "specs.json"); err == nil {
				t.Fatalf("Load should fail for %s", tc.name)
			} else if !strings.Contains(err.Error(), "modelregistry") {
				t.Errorf("error should be wrapped by package: %v", err)
			}
		})
	}
}

// TestLoad_MissingFile: arquivo ausente no FS é erro (não panic).
func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(fstest.MapFS{}, "specs.json"); err == nil {
		t.Fatal("Load should fail when specs.json is missing")
	}
}
