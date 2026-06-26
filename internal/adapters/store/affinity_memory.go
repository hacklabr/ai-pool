package store

import (
	"context"
	"sync"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
)

// memoryAffinityStore implementa domain.AffinityStore em memória, por usuário.
//
// Estrutura: userID -> { conversationKey -> affinityEntry }.
// TTL 30 min (default). LRU eviction por usuário (limite configurável, default 50).
//
// NÃO persistir fingerprint (LGPD, spec §3.3.7). Reinício do proxy perde o
// mapeamento — aceitável (afinidade é best-effort).
//
// Não armazena conteúdo das mensagens, apenas o conversationKey derivado
// (X-Conversation-ID ou hash SHA-256 truncado). O hash JAMAIS é logado.
type memoryAffinityStore struct {
	mu         sync.Mutex
	byUser     map[domain.ID]*userAffinity
	ttl        time.Duration
	maxPerUser int
	now        func() time.Time // injetável para testes
}

type userAffinity struct {
	entries map[string]*affinityEntry // conversationKey -> entry
	// Para LRU eviction por usuário: mantemos uma lista implícita ordenada por
	// lastSeen. Simplicidade > performance neste volume (≤50 entries/usuário).
}

type affinityEntry struct {
	keyID        domain.ID
	messageCount int
	lastSeen     time.Time
	expiresAt    time.Time
}

// AffinityOption configura o store no construtor.
type AffinityOption func(*memoryAffinityStore)

// WithAffinityTTL sobrescreve o TTL default (30min).
func WithAffinityTTL(d time.Duration) AffinityOption {
	return func(s *memoryAffinityStore) { s.ttl = d }
}

// WithAffinityMaxPerUser sobrescreve o limite por usuário (default 50).
func WithAffinityMaxPerUser(n int) AffinityOption {
	return func(s *memoryAffinityStore) { s.maxPerUser = n }
}

// NewAffinityStore cria um AffinityStore in-memory com defaults do spec.
func NewAffinityStore(opts ...AffinityOption) domain.AffinityStore {
	s := &memoryAffinityStore{
		byUser:     make(map[domain.ID]*userAffinity),
		ttl:        30 * time.Minute,
		maxPerUser: 50,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Lookup tenta encontrar a chave de afinidade. Aplica expiry + invalidação
// lazy na leitura.
func (s *memoryAffinityStore) Lookup(_ context.Context, userID domain.ID, conversationKey string) (domain.ID, error) {
	if conversationKey == "" {
		return 0, domain.ErrNoAffinity
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ua, ok := s.byUser[userID]
	if !ok {
		return 0, domain.ErrNoAffinity
	}
	e, ok := ua.entries[conversationKey]
	if !ok {
		return 0, domain.ErrNoAffinity
	}
	now := s.now()
	if now.After(e.expiresAt) {
		// Expirou — limpa lazy.
		delete(ua.entries, conversationKey)
		return 0, domain.ErrNoAffinity
	}
	e.lastSeen = now
	return e.keyID, nil
}

// Remember registra/atualiza a afinidade. Renova TTL. Aplica LRU eviction se
// exceder maxPerUser.
func (s *memoryAffinityStore) Remember(_ context.Context, userID domain.ID, conversationKey string, keyID domain.ID, messageCount int) error {
	if conversationKey == "" {
		return nil // nothing to remember
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ua, ok := s.byUser[userID]
	if !ok {
		ua = &userAffinity{entries: make(map[string]*affinityEntry)}
		s.byUser[userID] = ua
	}
	now := s.now()
	expiresAt := now.Add(s.ttl)

	if existing, ok := ua.entries[conversationKey]; ok {
		existing.keyID = keyID
		existing.messageCount = messageCount
		existing.lastSeen = now
		existing.expiresAt = expiresAt
		return nil
	}

	// Eviction LRU se excedeu limite.
	if len(ua.entries) >= s.maxPerUser {
		s.evictOldestLocked(ua)
	}

	ua.entries[conversationKey] = &affinityEntry{
		keyID:        keyID,
		messageCount: messageCount,
		lastSeen:     now,
		expiresAt:    expiresAt,
	}
	return nil
}

// Invalidate remove uma entrada específica (ex.: chave alvo foi desabilitada).
func (s *memoryAffinityStore) Invalidate(_ context.Context, userID domain.ID, conversationKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ua, ok := s.byUser[userID]
	if !ok {
		return nil
	}
	delete(ua.entries, conversationKey)
	return nil
}

// evictOldestLocked remove a entrada mais antiga (lastSeen mínima).
// Caller DEVE segurar s.mu.
func (s *memoryAffinityStore) evictOldestLocked(ua *userAffinity) {
	var oldestKey string
	var oldestTS time.Time
	first := true
	for k, e := range ua.entries {
		if first || e.lastSeen.Before(oldestTS) {
			oldestKey = k
			oldestTS = e.lastSeen
			first = false
		}
	}
	if !first {
		delete(ua.entries, oldestKey)
	}
}

// DefaultConversationResolver implementa domain.ConversationResolver.
// Prioriza conversationID explícito sobre hash de prefixo.
type DefaultConversationResolver struct{}

// Resolve retorna o conversationKey apropriado. Empty string se nenhum disponível.
func (DefaultConversationResolver) Resolve(conversationID, normalizedPrefixHash string) string {
	if conversationID != "" {
		return "id:" + conversationID
	}
	if normalizedPrefixHash != "" {
		return "h:" + normalizedPrefixHash
	}
	return ""
}
