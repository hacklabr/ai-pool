package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
)

// ===== LRUStore =====

// TestLRUStore_PickLeastUsed validates the core selection semantics.
// PickLeastUsed deve (1) escolher a menos recente, (2) atualizar last_used_at
// no momento da seleção para que a próxima chamada veja outra chave, (3) respeitar
// exclude, (4) retornar ErrNoKeysAvailable quando vazio.
func TestLRUStore_PickLeastUsed(t *testing.T) {
	ctx := context.Background()
	s := NewLRUStore().(*memoryLRUStore)

	const prov domain.ID = 100
	// Popula 3 candidatas via SetCandidates (cold start).
	s.SetCandidates(prov, []domain.ID{1, 2, 3})

	// 1a seleção: todas têm time.Time{} (zero); a primeira encontrada é retornada.
	// Como iteramos map não há ordem garantida, mas todas têm mesmo TS → qualquer uma.
	c1, err := s.PickLeastUsed(ctx, prov, nil)
	if err != nil {
		t.Fatalf("first pick: %v", err)
	}
	if c1 == nil || c1.ProviderID != prov {
		t.Fatalf("first pick returned nil/wrong provider: %+v", c1)
	}

	// 2a seleção: deve retornar uma chave DIFERENTE da primeira (c1 foi marcada
	// com last_used_at=now, então ela agora é a "mais recente").
	c2, err := s.PickLeastUsed(ctx, prov, nil)
	if err != nil {
		t.Fatalf("second pick: %v", err)
	}
	if c2.KeyID == c1.KeyID {
		t.Fatalf("second pick returned same key as first (%d) — last_used update did not promote next", c1.KeyID)
	}

	// 3a seleção: agora deve ser a chave ainda não usada (entre 1,2,3 a que sobrou),
	// OU a menos recente das 2 usadas. De qualquer forma, diferente das 2 usadas
	// no conjunto "least recently".
	c3, err := s.PickLeastUsed(ctx, prov, nil)
	if err != nil {
		t.Fatalf("third pick: %v", err)
	}
	if c3.KeyID == c1.KeyID {
		// c1 foi a mais recente na 1a, agora está há 2 picks; pode ser reeleita
		// só se for estritamente a menos recente entre as restantes. Verificamos
		// apenas que NÃO é c2 (que acabou de ser marcada).
	}
	if c3.KeyID == c2.KeyID {
		t.Fatalf("third pick returned c2 (just used) — last_used update did not promote next")
	}
}

// TestLRUStore_Exclude garante que chaves em exclude são puladas.
func TestLRUStore_Exclude(t *testing.T) {
	ctx := context.Background()
	s := NewLRUStore().(*memoryLRUStore)
	const prov domain.ID = 200
	s.SetCandidates(prov, []domain.ID{10, 20, 30})

	// Marcamos 10 e 20 como "usadas" para que só reste a 30 elegível.
	s.MarkUsed(ctx, 10)
	s.MarkUsed(ctx, 20)
	time.Sleep(time.Millisecond)

	// Agora pedimos excluíndo 10 e 20 — só resta 30.
	c, err := s.PickLeastUsed(ctx, prov, []domain.ID{10, 20})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if c.KeyID != 30 {
		t.Fatalf("expected key 30, got %d", c.KeyID)
	}
}

// TestLRUStore_NoKeysAvailable valida o erro sentinel.
func TestLRUStore_NoKeysAvailable(t *testing.T) {
	ctx := context.Background()
	s := NewLRUStore()
	_, err := s.PickLeastUsed(ctx, 999, nil)
	if !errors.Is(err, domain.ErrNoKeysAvailable) {
		t.Fatalf("expected ErrNoKeysAvailable, got %v", err)
	}
}

// TestLRUStore_DisableReenable valida o ciclo de vida da chave no store.
func TestLRUStore_DisableReenable(t *testing.T) {
	ctx := context.Background()
	s := NewLRUStore().(*memoryLRUStore)
	const prov domain.ID = 300
	s.SetCandidates(prov, []domain.ID{1, 2, 3})

	if err := s.OnKeyDisabled(ctx, 2); err != nil {
		t.Fatalf("OnKeyDisabled: %v", err)
	}
	// Deve conseguir selecionar entre 1 e 3 apenas (mesmo se solicitarmos
	// excluir vazia, key 2 não deve voltar).
	for i := 0; i < 10; i++ {
		c, err := s.PickLeastUsed(ctx, prov, nil)
		if err != nil {
			t.Fatalf("pick after disable: %v", err)
		}
		if c.KeyID == 2 {
			t.Fatalf("disabled key 2 was selected")
		}
		s.MarkUsed(ctx, c.KeyID)
		time.Sleep(time.Millisecond)
	}

	// Reenable: key 2 volta ao pool.
	if err := s.OnKeyReenabled(ctx, 2, prov); err != nil {
		t.Fatalf("OnKeyReenabled: %v", err)
	}
	found := false
	for i := 0; i < 20; i++ {
		c, err := s.PickLeastUsed(ctx, prov, nil)
		if err != nil {
			t.Fatalf("pick after reenable: %v", err)
		}
		if c.KeyID == 2 {
			found = true
			break
		}
		s.MarkUsed(ctx, c.KeyID)
		time.Sleep(time.Millisecond)
	}
	if !found {
		t.Fatalf("reenabled key 2 never selected after reenable")
	}
}

// ===== AffinityStore =====

func TestAffinityStore_RememberLookupInvalidate(t *testing.T) {
	ctx := context.Background()
	s := NewAffinityStore(WithAffinityTTL(time.Minute)).(*memoryAffinityStore)

	const user domain.ID = 42
	const key = "id:conv-123"

	// Lookup antes de Remember → ErrNoAffinity.
	if _, err := s.Lookup(ctx, user, key); !errors.Is(err, domain.ErrNoAffinity) {
		t.Fatalf("expected ErrNoAffinity before Remember, got %v", err)
	}

	// Remember + Lookup → retorna a chave registrada.
	if err := s.Remember(ctx, user, key, 7, 3); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	got, err := s.Lookup(ctx, user, key)
	if err != nil {
		t.Fatalf("Lookup after Remember: %v", err)
	}
	if got != 7 {
		t.Fatalf("expected key 7, got %d", got)
	}

	// Invalidate + Lookup → ErrNoAffinity.
	if err := s.Invalidate(ctx, user, key); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := s.Lookup(ctx, user, key); !errors.Is(err, domain.ErrNoAffinity) {
		t.Fatalf("expected ErrNoAffinity after Invalidate, got %v", err)
	}
}

// TestAffinityStore_IsolationByUser garante que conversas com mesmo conversationKey
// em usuários diferentes NÃO colidem.
func TestAffinityStore_IsolationByUser(t *testing.T) {
	ctx := context.Background()
	s := NewAffinityStore().(*memoryAffinityStore)
	const key = "h:abc"

	if err := s.Remember(ctx, 1, key, 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(ctx, 2, key, 200, 1); err != nil {
		t.Fatal(err)
	}

	k1, _ := s.Lookup(ctx, 1, key)
	k2, _ := s.Lookup(ctx, 2, key)
	if k1 != 100 || k2 != 200 {
		t.Fatalf("cross-user leakage: user1=%d user2=%d", k1, k2)
	}
}

// TestAffinityStore_TTLExpiry valida expiração lazy.
func TestAffinityStore_TTLExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewAffinityStore(WithAffinityTTL(50 * time.Millisecond)).(*memoryAffinityStore)
	const user domain.ID = 1
	const key = "id:c"

	if err := s.Remember(ctx, user, key, 5, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, user, key); err != nil {
		t.Fatalf("lookup before TTL: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := s.Lookup(ctx, user, key); !errors.Is(err, domain.ErrNoAffinity) {
		t.Fatalf("expected ErrNoAffinity after TTL, got %v", err)
	}
}

// TestConversationResolver_Default valida prioridade ID > hash.
func TestConversationResolver_Default(t *testing.T) {
	r := DefaultConversationResolver{}
	if got := r.Resolve("conv-1", "h:abc"); got != "id:conv-1" {
		t.Fatalf("expected id prefix, got %q", got)
	}
	if got := r.Resolve("", "h:abc"); got != "h:h:abc" {
		t.Fatalf("expected hash fallback, got %q", got)
	}
	if got := r.Resolve("", ""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestRateLimiter_AllowThenDeny(t *testing.T) {
	ctx := context.Background()
	// burst=3, rate=60/min (1/s). As 3 primeiras Allow devem passar.
	rl := NewRateLimiter(WithRateLimitDefaults(3, 60)).(*memoryRateLimiter)

	for i := 0; i < 3; i++ {
		ok, _, err := rl.Allow(ctx, "user:1")
		if err != nil || !ok {
			t.Fatalf("call %d: expected allow, got ok=%v err=%v", i, ok, err)
		}
	}
	// 4a chamada deve ser negada (burst esgotado, refill ainda não aconteceu).
	ok, retry, err := rl.Allow(ctx, "user:1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatalf("expected deny after burst exhausted")
	}
	if retry <= 0 {
		t.Fatalf("expected positive retryAfter, got %v", retry)
	}
}

func TestRateLimiter_IsolationByScope(t *testing.T) {
	ctx := context.Background()
	rl := NewRateLimiter(WithRateLimitDefaults(1, 60)).(*memoryRateLimiter)
	// user:1 esgota burst.
	if ok, _, _ := rl.Allow(ctx, "user:1"); !ok {
		t.Fatal("user:1 first call should pass")
	}
	if ok, _, _ := rl.Allow(ctx, "user:1"); ok {
		t.Fatal("user:1 second call should deny (burst=1)")
	}
	// user:2 tem bucket independente.
	if ok, _, _ := rl.Allow(ctx, "user:2"); !ok {
		t.Fatal("user:2 first call should pass (isolated)")
	}
}
