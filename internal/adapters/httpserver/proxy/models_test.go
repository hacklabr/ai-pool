package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Testes do endpoint agregado GET /v1/models (RF-12).
//
// Cobrem: 401 sem token, 200 com lista agregada + dedup por nome (primeiro
// provider ativo vence), ?format=opencode com limit/modalities, 503 sem
// catálogo configurado, e modelos de providers inativos excluídos.
// =============================================================================

// denyAllValidator rejeita qualquer token (401).
type denyAllValidator struct{}

func (denyAllValidator) Validate(_ context.Context, _ string) (*domain.User, *domain.APIToken, error) {
	return nil, nil, domain.ErrTokenInvalid
}

type fakeModelCatalogLister struct {
	models []domain.Model
	err    error
}

func (f fakeModelCatalogLister) ListAll(_ context.Context, _ domain.OrganizationID) ([]domain.Model, error) {
	return f.models, f.err
}

type fakeActiveProviderLister struct {
	providers []domain.Provider
	err       error
}

func (f fakeActiveProviderLister) ListActive(_ context.Context, _ domain.OrganizationID) ([]domain.Provider, error) {
	return f.providers, f.err
}

func intPtr(v int) *int { return &v }

func strPtr(v string) *string { return &v }

// catalogFixture monta um catálogo com:
//   - provider 2 "alpha" (id menor... não: id 2) e provider 5 "beta"
//   - "shared-model" nos DOIS providers → dedup: alpha (menor id) vence
//   - "vision-model" multimodal com limit (context+output)
//   - "inactive-model" inativo → fora
//   - "orphan-model" de provider INATIVO (não listado) → fora
func catalogFixture() ([]domain.Model, []domain.Provider) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	models := []domain.Model{
		{ID: 1, ProviderID: 5, OrgID: 1, Name: "shared-model", IsActive: true, CreatedAt: created},
		{ID: 2, ProviderID: 2, OrgID: 1, Name: "shared-model", IsActive: true, CreatedAt: created},
		{ID: 3, ProviderID: 2, OrgID: 1, Name: "vision-model", DisplayName: strPtr("Vision Model"),
			ContextWindow: intPtr(128000), OutputLimit: intPtr(8192),
			Modalities: []string{"text", "image"}, IsActive: true, CreatedAt: created},
		{ID: 4, ProviderID: 2, OrgID: 1, Name: "inactive-model", IsActive: false, CreatedAt: created},
		{ID: 5, ProviderID: 9, OrgID: 1, Name: "orphan-model", IsActive: true, CreatedAt: created},
	}
	providers := []domain.Provider{
		{ID: 5, OrgID: 1, Slug: "beta", IsActive: true},
		{ID: 2, OrgID: 1, Slug: "alpha", IsActive: true},
	}
	return models, providers
}

func newModelsTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newTestServer(t, "http://unused")
	models, providers := catalogFixture()
	srv.SetModelCatalog(fakeModelCatalogLister{models: models}, fakeActiveProviderLister{providers: providers})
	return srv
}

func doModelsRequest(t *testing.T, srv *Server, path, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestListModels_Unauthorized: sem token → 401 (mesmo AuthMiddleware do proxy).
func TestListModels_Unauthorized(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused")
	srv.tokenValidator = denyAllValidator{}
	srv.SetModelCatalog(fakeModelCatalogLister{}, fakeActiveProviderLister{})

	for _, path := range []string{"/v1/models", "/api/models"} {
		rec := doModelsRequest(t, srv, path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s sem token: status = %d, want 401", path, rec.Code)
		}
		rec = doModelsRequest(t, srv, path, "Bearer sk-aipp-invalid")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s token inválido: status = %d, want 401", path, rec.Code)
		}
	}
}

// TestListModels_OpenAIFormat: lista agregada com dedup por nome; primeiro
// provider ativo (menor id) vence o owned_by.
func TestListModels_OpenAIFormat(t *testing.T) {
	srv := newModelsTestServer(t)
	rec := doModelsRequest(t, srv, "/v1/models", "Bearer sk-aipp-valid")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want list", body.Object)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 (dedup + só ativos de providers ativos): %+v", len(body.Data), body.Data)
	}
	byID := map[string]string{}
	for _, m := range body.Data {
		if m.Object != "model" {
			t.Errorf("entry %q object = %q, want model", m.ID, m.Object)
		}
		if m.Created <= 0 {
			t.Errorf("entry %q created = %d, want unix timestamp > 0", m.ID, m.Created)
		}
		byID[m.ID] = m.OwnedBy
	}
	if byID["shared-model"] != "alpha" {
		t.Errorf("shared-model owned_by = %q, want alpha (primeiro provider vence)", byID["shared-model"])
	}
	if byID["vision-model"] != "alpha" {
		t.Errorf("vision-model owned_by = %q, want alpha", byID["vision-model"])
	}
	if _, ok := byID["inactive-model"]; ok {
		t.Error("inactive-model should not be listed")
	}
	if _, ok := byID["orphan-model"]; ok {
		t.Error("orphan-model (provider inativo) should not be listed")
	}
}

// TestListModels_ApiAlias: /api/models responde o mesmo conteúdo.
func TestListModels_ApiAlias(t *testing.T) {
	srv := newModelsTestServer(t)
	rec := doModelsRequest(t, srv, "/api/models", "Bearer sk-aipp-valid")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestListModels_OpenCodeFormat: ?format=opencode devolve o mapa no schema
// OpenCode com limit e modalities.
func TestListModels_OpenCodeFormat(t *testing.T) {
	srv := newModelsTestServer(t)
	rec := doModelsRequest(t, srv, "/v1/models?format=opencode", "Bearer sk-aipp-valid")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(body))
	}

	vision, ok := body["vision-model"]
	if !ok {
		t.Fatal("vision-model missing from opencode map")
	}
	if vision["name"] != "Vision Model" {
		t.Errorf("vision-model name = %v, want DisplayName 'Vision Model'", vision["name"])
	}
	limit, ok := vision["limit"].(map[string]any)
	if !ok {
		t.Fatal("vision-model missing limit block")
	}
	if limit["context"] != float64(128000) {
		t.Errorf("limit.context = %v, want 128000", limit["context"])
	}
	if limit["output"] != float64(8192) {
		t.Errorf("limit.output = %v, want 8192", limit["output"])
	}
	modalities, ok := vision["modalities"].(map[string]any)
	if !ok {
		t.Fatal("vision-model missing modalities block")
	}
	input, ok := modalities["input"].([]any)
	if !ok || len(input) != 2 {
		t.Errorf("modalities.input = %v, want [text image]", modalities["input"])
	}

	// Modelo sem enriquecimento: só o campo name.
	shared, ok := body["shared-model"]
	if !ok {
		t.Fatal("shared-model missing from opencode map")
	}
	if _, hasLimit := shared["limit"]; hasLimit {
		t.Error("shared-model should not have limit block (no enrichment)")
	}
	if _, hasMod := shared["modalities"]; hasMod {
		t.Error("shared-model should not have modalities block (text-only)")
	}
}

// TestListModels_NoCatalog: sem catálogo configurado (dev sem DB) → 503.
func TestListModels_NoCatalog(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused")
	rec := doModelsRequest(t, srv, "/v1/models", "Bearer sk-aipp-valid")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestListModels_RepoError: falha de DB → 500 (formato OpenAI).
func TestListModels_RepoError(t *testing.T) {
	srv, _ := newTestServer(t, "http://unused")
	srv.SetModelCatalog(
		fakeModelCatalogLister{err: errors.New("db down")},
		fakeActiveProviderLister{},
	)
	rec := doModelsRequest(t, srv, "/v1/models", "Bearer sk-aipp-valid")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}
