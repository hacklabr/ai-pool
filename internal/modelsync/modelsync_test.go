package modelsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Testes da reconciliação do catálogo (RF-11, ADR-0015) — table-driven.
//
// Cobrem: modelo novo (added), reativação, desativação, inalterado, fail-safe
// (provider fora do ar / sem chave / JSON inválido / HTTP != 200 → zero
// alterações) e preservação de enriquecimento manual (nunca Update).
// =============================================================================

// --- Fakes das portas ---

type fakeProviderLister struct {
	providers []domain.Provider
	err       error
}

func (f fakeProviderLister) ListActive(_ context.Context, _ domain.OrganizationID) ([]domain.Provider, error) {
	return f.providers, f.err
}

type fakeKeyLister struct {
	keys map[domain.ID][]domain.ProviderKey
	err  error
}

func (f fakeKeyLister) ListUsableByProvider(_ context.Context, _ domain.OrganizationID, providerID domain.ID) ([]domain.ProviderKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.keys[providerID], nil
}

type fakeDecryptor struct{ err error }

func (f fakeDecryptor) Decrypt(_ context.Context, key domain.ProviderKey) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte("sk-provider-secret"), nil
}

// setActiveCall registra chamadas de SetActive (id, active).
type setActiveCall struct {
	id     domain.ID
	active bool
}

type fakeModelStore struct {
	models    []domain.Model
	created   []domain.Model
	setActive []setActiveCall
	createErr error
	listErr   error
}

func (f *fakeModelStore) ListAll(_ context.Context, _ domain.OrganizationID) ([]domain.Model, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.models, nil
}

func (f *fakeModelStore) Create(_ context.Context, m *domain.Model) (domain.ID, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.created = append(f.created, *m)
	return domain.ID(1000 + len(f.created)), nil
}

func (f *fakeModelStore) SetActive(_ context.Context, _ domain.OrganizationID, id domain.ID, active bool) error {
	f.setActive = append(f.setActive, setActiveCall{id: id, active: active})
	return nil
}

// --- Helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func activeProvider(id domain.ID, slug, baseURL string) domain.Provider {
	return domain.Provider{
		ID:       id,
		OrgID:    domain.DefaultOrganizationID,
		Name:     slug,
		Slug:     slug,
		BaseURL:  baseURL,
		APISpec:  "openai",
		IsActive: true,
	}
}

func usableKey(id, providerID domain.ID) domain.ProviderKey {
	return domain.ProviderKey{
		ID:         id,
		ProviderID: providerID,
		OrgID:      domain.DefaultOrganizationID,
		Label:      "k",
		Status:     domain.KeyStatusEnabled,
	}
}

func localModel(id, providerID domain.ID, name string, active bool) domain.Model {
	return domain.Model{
		ID:         id,
		ProviderID: providerID,
		OrgID:      domain.DefaultOrganizationID,
		Name:       name,
		IsActive:   active,
	}
}

// upstreamServer serve a listagem de modelos no formato OpenAI.
func upstreamServer(t *testing.T, status int, body string, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		if wantAuth != "" {
			if got := r.Header.Get("Authorization"); got != wantAuth {
				t.Errorf("upstream Authorization = %q, want %q", got, wantAuth)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func newTestSyncer(providers []domain.Provider, keys map[domain.ID][]domain.ProviderKey, store *fakeModelStore) *Syncer {
	return NewSyncer(
		fakeProviderLister{providers: providers},
		fakeKeyLister{keys: keys},
		store,
		fakeDecryptor{},
		http.DefaultClient,
		testLogger(),
		domain.DefaultOrganizationID,
	)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := sortedStrings(a), sortedStrings(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// --- Testes de reconciliação (table-driven) ---

func TestSyncAll_Reconciliation(t *testing.T) {
	okBody := `{"object":"list","data":[{"id":"m-new"},{"id":"m-active"},{"id":"m-inactive"},` +
		`{"no_id":true},{"id":""}]}` // entradas sem id são ignoradas

	tests := []struct {
		name            string
		local           []domain.Model
		upstreamStatus  int
		upstreamBody    string
		wantAdded       []string
		wantReactivated []string
		wantDeactivated []string
		wantSkipped     bool
		wantCreates     int
		wantSetActive   int
	}{
		{
			name: "new model is added active",
			local: []domain.Model{
				localModel(1, 10, "m-active", true),
				localModel(2, 10, "m-inactive", false),
			},
			upstreamStatus:  200,
			upstreamBody:    okBody,
			wantAdded:       []string{"m-new"},
			wantReactivated: []string{"m-inactive"},
			wantCreates:     1,
			wantSetActive:   1,
		},
		{
			name: "missing model is deactivated",
			local: []domain.Model{
				localModel(1, 10, "m-active", true),
				localModel(3, 10, "m-gone", true),
			},
			upstreamStatus:  200,
			upstreamBody:    `{"data":[{"id":"m-active"}]}`,
			wantDeactivated: []string{"m-gone"},
			wantSetActive:   1,
		},
		{
			name: "unchanged when catalog matches upstream",
			local: []domain.Model{
				localModel(1, 10, "m-active", true),
			},
			upstreamStatus: 200,
			upstreamBody:   `{"data":[{"id":"m-active"}]}`,
		},
		{
			name: "already inactive missing model stays untouched",
			local: []domain.Model{
				localModel(3, 10, "m-gone", false),
			},
			upstreamStatus: 200,
			upstreamBody:   `{"data":[{"id":"m-active"}]}`,
			wantAdded:      []string{"m-active"},
			wantCreates:    1,
		},
		{
			name:           "upstream failure is fail-safe (zero changes)",
			local:          []domain.Model{localModel(1, 10, "m-active", true)},
			upstreamStatus: 500,
			upstreamBody:   `{"error":"boom"}`,
			wantSkipped:    true,
		},
		{
			name:           "invalid JSON is fail-safe (zero changes)",
			local:          []domain.Model{localModel(1, 10, "m-active", true)},
			upstreamStatus: 200,
			upstreamBody:   `not json at all`,
			wantSkipped:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := upstreamServer(t, tc.upstreamStatus, tc.upstreamBody, "Bearer sk-provider-secret")
			defer upstream.Close()

			// base_url contém a versão (/v1) — o sync anexa só "/models".
			provider := activeProvider(10, "testprov", upstream.URL+"/v1")
			store := &fakeModelStore{models: tc.local}
			syncer := newTestSyncer(
				[]domain.Provider{provider},
				map[domain.ID][]domain.ProviderKey{10: {usableKey(100, 10)}},
				store,
			)

			report, err := syncer.SyncAll(context.Background())
			if err != nil {
				t.Fatalf("SyncAll returned structural error: %v", err)
			}
			if len(report.Providers) != 1 {
				t.Fatalf("report has %d providers, want 1", len(report.Providers))
			}
			rep := report.Providers[0]

			if rep.Skipped != tc.wantSkipped {
				t.Errorf("skipped = %v (reason %q), want %v", rep.Skipped, rep.SkipReason, tc.wantSkipped)
			}
			if !equalStrings(rep.Added, tc.wantAdded) {
				t.Errorf("added = %v, want %v", rep.Added, tc.wantAdded)
			}
			if !equalStrings(rep.Reactivated, tc.wantReactivated) {
				t.Errorf("reactivated = %v, want %v", rep.Reactivated, tc.wantReactivated)
			}
			if !equalStrings(rep.Deactivated, tc.wantDeactivated) {
				t.Errorf("deactivated = %v, want %v", rep.Deactivated, tc.wantDeactivated)
			}
			if len(store.created) != tc.wantCreates {
				t.Errorf("Create calls = %d, want %d", len(store.created), tc.wantCreates)
			}
			if len(store.setActive) != tc.wantSetActive {
				t.Errorf("SetActive calls = %d, want %d", len(store.setActive), tc.wantSetActive)
			}

			// Modelos criados: Name=id, DisplayName=id, IsActive=true,
			// enriquecimento nulo (sync só traz o id).
			for _, created := range store.created {
				if created.DisplayName == nil || *created.DisplayName != created.Name {
					t.Errorf("created model %q: DisplayName = %v, want %q", created.Name, created.DisplayName, created.Name)
				}
				if !created.IsActive {
					t.Errorf("created model %q: IsActive = false, want true", created.Name)
				}
				if created.ContextWindow != nil || created.OutputLimit != nil || len(created.Modalities) != 0 {
					t.Errorf("created model %q must not carry enrichment fields", created.Name)
				}
			}
		})
	}
}

// TestSyncAll_NoUsableKey: provedor sem chave usável → skipped, zero alterações.
func TestSyncAll_NoUsableKey(t *testing.T) {
	upstream := upstreamServer(t, 200, `{"data":[{"id":"m1"}]}`, "")
	defer upstream.Close()

	provider := activeProvider(10, "testprov", upstream.URL)
	store := &fakeModelStore{models: []domain.Model{localModel(1, 10, "m-active", true)}}
	syncer := newTestSyncer(
		[]domain.Provider{provider},
		map[domain.ID][]domain.ProviderKey{10: {}}, // sem chaves
		store,
	)

	report, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	rep := report.Providers[0]
	if !rep.Skipped || rep.SkipReason != "no usable key" {
		t.Errorf("rep = %+v, want skipped with reason 'no usable key'", rep)
	}
	if len(store.created) != 0 || len(store.setActive) != 0 {
		t.Errorf("expected zero changes, got created=%d setActive=%d", len(store.created), len(store.setActive))
	}
}

// TestSyncAll_NetworkError: provedor fora do ar → skipped, zero alterações,
// e os DEMAIS provedores continuam sendo sincronizados (fail-safe isolado).
func TestSyncAll_NetworkErrorDoesNotAffectOtherProviders(t *testing.T) {
	upstreamOK := upstreamServer(t, 200, `{"data":[{"id":"m-ok"}]}`, "")
	defer upstreamOK.Close()
	// URL que recusa conexão (porta fechada).
	deadURL := "http://127.0.0.1:1"

	dead := activeProvider(10, "dead", deadURL)
	ok := activeProvider(20, "ok", upstreamOK.URL+"/v1")
	store := &fakeModelStore{models: []domain.Model{
		localModel(1, 10, "m-active", true),
	}}
	syncer := newTestSyncer(
		[]domain.Provider{dead, ok},
		map[domain.ID][]domain.ProviderKey{
			10: {usableKey(100, 10)},
			20: {usableKey(200, 20)},
		},
		store,
	)

	report, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if len(report.Providers) != 2 {
		t.Fatalf("report has %d providers, want 2", len(report.Providers))
	}
	if !report.Providers[0].Skipped {
		t.Errorf("dead provider should be skipped, got %+v", report.Providers[0])
	}
	if report.Providers[1].Skipped {
		t.Errorf("healthy provider should NOT be skipped, got %+v", report.Providers[1])
	}
	if !equalStrings(report.Providers[1].Added, []string{"m-ok"}) {
		t.Errorf("healthy provider added = %v, want [m-ok]", report.Providers[1].Added)
	}
	// O modelo do provedor morto NÃO foi desativado (fail-safe).
	for _, call := range store.setActive {
		if call.id == 1 && !call.active {
			t.Error("model of unreachable provider was deactivated — fail-safe violated")
		}
	}
}

// TestSyncAll_NoProviders: sem providers ativos → relatório vazio, sem erro
// (CLI sai 0 — "nada a fazer").
func TestSyncAll_NoProviders(t *testing.T) {
	store := &fakeModelStore{}
	syncer := newTestSyncer(nil, nil, store)

	report, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if len(report.Providers) != 0 {
		t.Errorf("report has %d providers, want 0", len(report.Providers))
	}
}

// TestSyncAll_StructuralErrors: falha de DB ao listar providers/modelos →
// erro estrutural (CLI sai 1).
func TestSyncAll_StructuralErrors(t *testing.T) {
	t.Run("list providers fails", func(t *testing.T) {
		syncer := NewSyncer(
			fakeProviderLister{err: errors.New("db down")},
			fakeKeyLister{},
			&fakeModelStore{},
			fakeDecryptor{},
			http.DefaultClient,
			testLogger(),
			domain.DefaultOrganizationID,
		)
		if _, err := syncer.SyncAll(context.Background()); err == nil {
			t.Error("SyncAll should return structural error when ListActive fails")
		}
	})
	t.Run("list models fails", func(t *testing.T) {
		syncer := NewSyncer(
			fakeProviderLister{providers: []domain.Provider{activeProvider(10, "p", "http://x")}},
			fakeKeyLister{},
			&fakeModelStore{listErr: errors.New("db down")},
			fakeDecryptor{},
			http.DefaultClient,
			testLogger(),
			domain.DefaultOrganizationID,
		)
		if _, err := syncer.SyncAll(context.Background()); err == nil {
			t.Error("SyncAll should return structural error when ListAll fails")
		}
	})
}

// TestSyncAll_DecryptError: falha ao decriptar a chave → skipped, zero alterações.
func TestSyncAll_DecryptError(t *testing.T) {
	upstream := upstreamServer(t, 200, `{"data":[{"id":"m1"}]}`, "")
	defer upstream.Close()

	store := &fakeModelStore{models: []domain.Model{localModel(1, 10, "m-active", true)}}
	syncer := NewSyncer(
		fakeProviderLister{providers: []domain.Provider{activeProvider(10, "p", upstream.URL)}},
		fakeKeyLister{keys: map[domain.ID][]domain.ProviderKey{10: {usableKey(100, 10)}}},
		store,
		fakeDecryptor{err: errors.New("kek missing")},
		http.DefaultClient,
		testLogger(),
		domain.DefaultOrganizationID,
	)

	report, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if !report.Providers[0].Skipped {
		t.Errorf("provider with undecryptable key should be skipped, got %+v", report.Providers[0])
	}
	if len(store.created) != 0 || len(store.setActive) != 0 {
		t.Error("expected zero changes when decrypt fails")
	}
}

// TestReport_String: formato de saída humana do relatório (CLI).
func TestReport_String(t *testing.T) {
	r := &Report{Providers: []ProviderReport{
		{ProviderSlug: "deepseek", Added: []string{"a", "b"}, Deactivated: []string{"c"}},
		{ProviderSlug: "mistral", Skipped: true, SkipReason: "no usable key"},
	}}
	out := r.String()
	for _, want := range []string{"deepseek", "added=2", "deactivated=1", "mistral", "no usable key"} {
		if !strings.Contains(out, want) {
			t.Errorf("report string missing %q:\n%s", want, out)
		}
	}

	empty := &Report{}
	if got := empty.String(); !strings.Contains(got, "nothing to do") {
		t.Errorf("empty report string = %q, want 'nothing to do'", got)
	}
}
