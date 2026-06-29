package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// newFakeClockSessionStore cria um store com clock fake injetável (para testes TTL).
func newFakeClockSessionStore(now func() time.Time) *memorySessionStore {
	return &memorySessionStore{
		sessions: make(map[string]*sessionEntry),
		now:      now,
	}
}

// TestSessionStore_CreateGetHappyPath valida o fluxo básico de Create→Get.
func TestSessionStore_CreateGetHappyPath(t *testing.T) {
	s := NewSessionStore().(*memorySessionStore)
	ctx := context.Background()

	sess := &domain.Session{
		ID:        "sess-1",
		UserID:    1,
		OrgID:     1,
		Username:  "tester",
		Role:      domain.RoleAdmin,
		CSRFToken: "csrf-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != 1 || got.Username != "tester" {
		t.Errorf("Get returned wrong data: %+v", got)
	}
	if got.CSRFToken != "csrf-1" {
		t.Errorf("CSRFToken = %q", got.CSRFToken)
	}
}

// TestSessionStore_GetNotFound retorna ErrSessionNotFound se inexistente.
func TestSessionStore_GetNotFound(t *testing.T) {
	s := NewSessionStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("esperava ErrSessionNotFound, got %v", err)
	}
}

// TestSessionStore_CreateCollision retorna erro se ID colide.
// (Improvável em produção com 32 bytes, mas validamos o contrato.)
func TestSessionStore_CreateCollision(t *testing.T) {
	s := NewSessionStore().(*memorySessionStore)
	ctx := context.Background()
	sess := &domain.Session{ID: "dup", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, sess); err == nil {
		t.Error("esperava erro em segunda Create com mesmo ID")
	}
}

// TestSessionStore_TTLExpiry valida lazy eviction em Get.
//
// Cenário: sessão expirou; Get deve retornar ErrSessionNotFound E remover a
// entrada do map interno.
func TestSessionStore_TTLExpiry(t *testing.T) {
	now := time.Now()
	s := newFakeClockSessionStore(func() time.Time { return now })

	sess := &domain.Session{
		ID:        "expiring",
		UserID:    1,
		ExpiresAt: now.Add(1 * time.Second),
	}
	_ = s.Create(context.Background(), sess)

	// Dentro do TTL: Get OK.
	if _, err := s.Get(context.Background(), "expiring"); err != nil {
		t.Fatalf("Get dentro do TTL: %v", err)
	}

	// Avança o clock para depois da expiração.
	now = now.Add(2 * time.Second)

	// Após TTL: Get retorna erro e remove entrada.
	_, err := s.Get(context.Background(), "expiring")
	if !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("esperava ErrSessionNotFound após TTL, got %v", err)
	}
	// Entrada deve ter sido removida (lazy eviction).
	s.mu.RLock()
	_, exists := s.sessions["expiring"]
	s.mu.RUnlock()
	if exists {
		t.Error("entrada expirada não foi removida (lazy eviction falhou)")
	}
}

// TestSessionStore_Delete idempotente.
func TestSessionStore_Delete(t *testing.T) {
	s := NewSessionStore().(*memorySessionStore)
	ctx := context.Background()
	_ = s.Create(ctx, &domain.Session{ID: "x", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if err := s.Delete(ctx, "x"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	// Segunda chamada em ID já removido: NÃO erro.
	if err := s.Delete(ctx, "x"); err != nil {
		t.Errorf("Delete idempotente falhou: %v", err)
	}
}

// TestSessionStore_DeleteAllForUser remove todas as sessões de um usuário.
// Cenário: desabilitar usuário invalida imediatamente todas as suas sessões.
func TestSessionStore_DeleteAllForUser(t *testing.T) {
	s := NewSessionStore().(*memorySessionStore)
	ctx := context.Background()
	// Usuário 1 tem 2 sessões; usuário 2 tem 1.
	_ = s.Create(ctx, &domain.Session{ID: "u1-a", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	_ = s.Create(ctx, &domain.Session{ID: "u1-b", UserID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	_ = s.Create(ctx, &domain.Session{ID: "u2-a", UserID: 2, ExpiresAt: time.Now().Add(time.Hour)})

	if err := s.DeleteAllForUser(ctx, 1); err != nil {
		t.Fatalf("DeleteAllForUser: %v", err)
	}

	// Sessões do user 1 não existem mais.
	if _, err := s.Get(ctx, "u1-a"); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("u1-a ainda existe: %v", err)
	}
	if _, err := s.Get(ctx, "u1-b"); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("u1-b ainda existe: %v", err)
	}
	// Sessão do user 2 permanece.
	if _, err := s.Get(ctx, "u2-a"); err != nil {
		t.Errorf("u2-a foi removida incorretamente: %v", err)
	}
}

// TestSessionStore_Cleanup ativo eviction remove só expiradas.
func TestSessionStore_Cleanup(t *testing.T) {
	now := time.Now()
	s := newFakeClockSessionStore(func() time.Time { return now })
	ctx := context.Background()
	_ = s.Create(ctx, &domain.Session{ID: "alive", UserID: 1, ExpiresAt: now.Add(time.Hour)})
	_ = s.Create(ctx, &domain.Session{ID: "dead1", UserID: 1, ExpiresAt: now.Add(-time.Minute)})
	_ = s.Create(ctx, &domain.Session{ID: "dead2", UserID: 2, ExpiresAt: now.Add(-time.Minute)})

	removed := s.Cleanup()
	if removed != 2 {
		t.Errorf("Cleanup removeu %d, want 2", removed)
	}
	// Alive ainda está.
	if _, err := s.Get(ctx, "alive"); err != nil {
		t.Errorf("alive foi removido incorretamente: %v", err)
	}
}

// TestSessionStore_Count retorna apenas ativas.
func TestSessionStore_Count(t *testing.T) {
	now := time.Now()
	s := newFakeClockSessionStore(func() time.Time { return now })
	ctx := context.Background()
	_ = s.Create(ctx, &domain.Session{ID: "a", UserID: 1, ExpiresAt: now.Add(time.Hour)})
	_ = s.Create(ctx, &domain.Session{ID: "b", UserID: 2, ExpiresAt: now.Add(time.Hour)})
	_ = s.Create(ctx, &domain.Session{ID: "c", UserID: 3, ExpiresAt: now.Add(-time.Minute)}) // expirada

	if n := s.Count(); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

// TestSessionStore_ConcurrentAccess garante thread-safety sob concorrência.
// Importante: 30 admins internos com sessões simultâneas.
func TestSessionStore_ConcurrentAccess(t *testing.T) {
	s := NewSessionStore().(*memorySessionStore)
	ctx := context.Background()

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i%26)) + string(rune('a'+i/26))
			_ = s.Create(ctx, &domain.Session{
				ID:        id,
				UserID:    domain.ID(i),
				ExpiresAt: time.Now().Add(time.Hour),
			})
			_, _ = s.Get(ctx, id)
			_ = s.Delete(ctx, id)
		}(i)
	}
	wg.Wait()
	// Sem data race detectável via -race.
}
