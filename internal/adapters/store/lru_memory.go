package store

import (
	"context"
	"sync"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// MemoryLRUStore implementa domain.LRUStore com map protegido por mutex.
//
// Estrutura por provider: map[keyID]lastUsed (atomic).
// Seleção: itera o map (n<=5 chaves por provider é trivial — O(n) é O(1) prático).
// Min-heap seria over-engineering (spec §4.4 do Backend Architect Turno 1).
//
// As candidatas (KeyCandidate) são fornecidas pelo repositório de chaves
// (PostgreSQL) via SeedCandidates na inicialização e em OnKeyReenabled.
// O store mantém APENAS os IDs e timestamps; o segredo descriptografado é
// injetado pelo repositório no momento da seleção.
//
// Dirty tracking: toda chave cujo last_used_at muda (PickLeastUsed ou MarkUsed)
// é registrada em `dirty`. O flusher periódico chama FlushDirty() para persistir
// os timestamps no DB (debounce 60s, spec §3.4.4).
type MemoryLRUStore struct {
	mu sync.Mutex
	// candidates por provider: providerID -> map[keyID]lastUsed
	byProvider map[domain.ID]map[domain.ID]time.Time
	// providerByKey: keyID -> providerID (para OnKeyDisabled saber a qual provider pertence)
	providerByKey map[domain.ID]domain.ID
	// dirty: keyID -> lastUsedAt (apenas chaves alteradas desde o último FlushDirty).
	// O flusher periódico lê e zera este map para persistir no DB.
	dirty map[domain.ID]time.Time
}

// NewLRUStore cria um LRUStore in-memory vazio.
// As candidatas devem ser populadas via SeedCandidates durante o boot
// (após carregar chaves ativas do DB) e via OnKeyReenabled quando reativadas.
func NewLRUStore() *MemoryLRUStore {
	return &MemoryLRUStore{
		byProvider:    make(map[domain.ID]map[domain.ID]time.Time),
		providerByKey: make(map[domain.ID]domain.ID),
		dirty:         make(map[domain.ID]time.Time),
	}
}

// SeedCandidates popula as candidatas iniciais por provider a partir dos
// timestamps do DB (boot). Deve ser chamado uma vez no boot após carregar
// chaves ativas do DB. Chaves com last_used_at NULL (nunca usadas) entram
// com time.Time{} (zero) — são candidatas a serem pickadas primeiro.
//
// Diferente de SetCandidates (que só aceita IDs), SeedCandidates aceita os
// timestamps reais do DB, permitindo reconstruir o ranking LRU após restart.
func (s *MemoryLRUStore) SeedCandidates(providerID domain.ID, seeds map[domain.ID]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time, len(seeds))
		s.byProvider[providerID] = m
	}
	for id, ts := range seeds {
		m[id] = ts
		s.providerByKey[id] = providerID
	}
}

// SetCandidates popula as candidatas iniciais por provider (compat testes).
// Chaves novas entram como "menos usadas" (lastUsed zero → NULLS FIRST).
func (s *MemoryLRUStore) SetCandidates(providerID domain.ID, keyIDs []domain.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time, len(keyIDs))
		s.byProvider[providerID] = m
	}
	for _, id := range keyIDs {
		if _, exists := m[id]; !exists {
			m[id] = time.Time{}
		}
		s.providerByKey[id] = providerID
	}
}

// PickLeastUsed seleciona a chave menos usada recentemente, excluindo as em
// `exclude` (chaves já tentadas no failover chain desta requisição).
//
// Semântica (spec §3.2.2): atualiza last_used_at=now ANTES de retornar, para
// que requisições simultâneas vejam a próxima menos recente.
//
// Não popula DecryptedValue/UserAgent — quem chama (orquestrador de failover)
// busca via repositório de chaves usando o KeyID retornado. Mantemos o store
// livre de segredos.
func (s *MemoryLRUStore) PickLeastUsed(_ context.Context, providerID domain.ID, exclude []domain.ID) (*domain.KeyCandidate, error) {
	excluded := make(map[domain.ID]struct{}, len(exclude))
	for _, id := range exclude {
		excluded[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.byProvider[providerID]
	if !ok || len(m) == 0 {
		return nil, domain.ErrNoKeysAvailable
	}

	var bestID domain.ID
	var bestTS time.Time
	var found bool
	now := time.Now()

	for id, ts := range m {
		if _, skip := excluded[id]; skip {
			continue
		}
		if !found || ts.Before(bestTS) {
			bestID = id
			bestTS = ts
			found = true
		}
	}
	if !found {
		return nil, domain.ErrNoKeysAvailable
	}

	// Atualiza no momento da seleção (semântica definida no spec).
	m[bestID] = now
	s.dirty[bestID] = now

	return &domain.KeyCandidate{KeyID: bestID, ProviderID: providerID}, nil
}

// MarkUsed atualiza last_used_at sem selecionar (usado após afinidade ou sucesso).
func (s *MemoryLRUStore) MarkUsed(_ context.Context, keyID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	providerID, ok := s.providerByKey[keyID]
	if !ok {
		return nil // chave não está no store; no-op
	}
	if m, ok := s.byProvider[providerID]; ok {
		if _, exists := m[keyID]; exists {
			now := time.Now()
			m[keyID] = now
			s.dirty[keyID] = now
		}
	}
	return nil
}

// OnKeyDisabled remove a chave do pool de candidatas (auto-disable).
// Permanece em providerByKey para que OnKeyReenabled saiba o provider.
func (s *MemoryLRUStore) OnKeyDisabled(_ context.Context, keyID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	providerID, ok := s.providerByKey[keyID]
	if !ok {
		return nil
	}
	if m, ok := s.byProvider[providerID]; ok {
		delete(m, keyID)
	}
	return nil
}

// OnKeyReenabled reinsere a chave no pool com last_used_at=agora (vai para o
// "fim" da fila LRU — é candidata a ser usada).
func (s *MemoryLRUStore) OnKeyReenabled(_ context.Context, keyID domain.ID, providerID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerByKey[keyID] = providerID
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time)
		s.byProvider[providerID] = m
	}
	now := time.Now()
	m[keyID] = now
	s.dirty[keyID] = now
	return nil
}

// EnsureCandidate adiciona a chave ao pool SÓ se ausente, com last_used_at=zero
// (candidata "fria" a ser pickada primeiro). NÃO toca em chaves já presentes —
// preserva o ranking LRU. Usa-se em cada request para incluir chaves cadastradas
// após o boot sem corromper o LRU (bug anterior: OnKeyReenabled resetava tudo).
func (s *MemoryLRUStore) EnsureCandidate(_ context.Context, keyID domain.ID, providerID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerByKey[keyID] = providerID
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time)
		s.byProvider[providerID] = m
	}
	if _, exists := m[keyID]; !exists {
		m[keyID] = time.Time{} // nunca usada → primeira da fila LRU
	}
	return nil
}

// FlushDirty retorna os timestamps dirty (keyID -> lastUsedAt) e zera o set.
// O flusher periódico chama este método para persistir last_used_at no DB.
// Retorna nil se não há entries dirty.
func (s *MemoryLRUStore) FlushDirty() map[domain.ID]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dirty) == 0 {
		return nil
	}
	out := s.dirty
	s.dirty = make(map[domain.ID]time.Time)
	return out
}
