package store

import (
	"context"
	"sync"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// memorySessionStore implementa domain.SessionStore com map protegido por
// RWMutex. Limpeza preguiçosa (lazy): entradas expiradas são removidas quando
// acessadas (Get) ou em flush periódico (ver Cleanup).
//
// Adequado para MVP single-réplica (~30 admins internos). Para multi-réplica
// (Opção B, ROADMAP), implementar RedisSessionStore sem mudanças no domínio.
type memorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry
	now      func() time.Time // injetável para testes
}

type sessionEntry struct {
	sess *domain.Session
	// indexByUser mantém um set auxiliar para DeleteAllForUser sem iterar tudo.
	// Implementação simples: iteramos tudo (n pequeno); otimização futura se
	// escala exigir.
}

// NewSessionStore cria um SessionStore in-memory.
func NewSessionStore() domain.SessionStore {
	return &memorySessionStore{
		sessions: make(map[string]*sessionEntry),
		now:      time.Now,
	}
}

// Create insere a sessão. Se já existe (colisão de ID, improvável), retorna erro.
func (m *memorySessionStore) Create(_ context.Context, s *domain.Session) error {
	if s == nil || s.ID == "" {
		return errInvalidSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[s.ID]; exists {
		return errSessionCollision
	}
	m.sessions[s.ID] = &sessionEntry{sess: s}
	return nil
}

// Get retorna a sessão se existe e não expirou. Lazy eviction em caso de
// expiração (a entrada é removida).
func (m *memorySessionStore) Get(_ context.Context, id string) (*domain.Session, error) {
	if id == "" {
		return nil, domain.ErrSessionNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.sessions[id]
	if !ok {
		return nil, domain.ErrSessionNotFound
	}
	if e.sess.IsExpired(m.now()) {
		// Lazy eviction.
		delete(m.sessions, id)
		return nil, domain.ErrSessionNotFound
	}
	// Sliding: renovamos LastSeenAt (mas NÃO estende ExpiresAt — TTL absoluto).
	e.sess.LastSeenAt = m.now()
	// Retorna CÓPIA defensiva para caller não mutar o estado interno.
	out := *e.sess
	return &out, nil
}

// Delete remove a sessão. Idempotente (não erro se não existe).
func (m *memorySessionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

// DeleteAllForUser remove todas as sessões do usuário. Usado em desabilitação
// de usuário (revogação imediata) e reset de senha.
func (m *memorySessionStore) DeleteAllForUser(_ context.Context, userID domain.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.sessions {
		if e.sess.UserID == userID {
			delete(m.sessions, id)
		}
	}
	return nil
}

// Cleanup remove TODAS as entradas expiradas (lazy + flush ativo).
//
// Para uso por um ticker periódico (ex.: a cada 10min) iniciado no main.go.
// Evita crescimento do map mesmo que nenhuma leitura ocorra nas entradas
// expiradas (pouco provável mas defensivo).
//
// Retorna o número de entradas removidas (para métricas/observabilidade).
func (m *memorySessionStore) Cleanup() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	removed := 0
	for id, e := range m.sessions {
		if e.sess.IsExpired(now) {
			delete(m.sessions, id)
			removed++
		}
	}
	return removed
}

// Count retorna o número de sessões ativas (não expiradas). Útil para métricas.
func (m *memorySessionStore) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now()
	n := 0
	for _, e := range m.sessions {
		if !e.sess.IsExpired(now) {
			n++
		}
	}
	return n
}

// Erros sentinelas locais.
type localErr string

func (e localErr) Error() string { return string(e) }

const (
	errInvalidSession   = localErr("session: nil session or empty id")
	errSessionCollision = localErr("session: id collision (extremely unlikely with 32 bytes entropy)")
)
