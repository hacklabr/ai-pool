package store

import (
	"context"
	"sync"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
)

// memoryLRUStore implementa domain.LRUStore com map protegido por mutex.
//
// Estrutura por provider: map[keyID]lastUsed (atomic).
// Seleção: itera o map (n≤5 chaves por provider é trivial — O(n) é O(1) prático).
// Min-heap seria over-engineering (spec §4.4 do Backend Architect Turno 1).
//
// As candidatas (KeyCandidate) são fornecidas pelo repositório de chaves
// (PostgreSQL) via SetCandidates na inicialização e em OnKeyReenabled.
// O store mantém APENAS os IDs e timestamps; o segredo descriptografado é
// injetado pelo repositório no momento da seleção.
type memoryLRUStore struct {
	mu sync.Mutex
	// candidates por provider: providerID -> map[keyID]lastUsed
	byProvider map[domain.ID]map[domain.ID]time.Time
	// providerByKey: keyID -> providerID (para OnKeyDisabled saber a qual provider pertence)
	providerByKey map[domain.ID]domain.ID
}

// NewLRUStore cria um LRUStore in-memory vazio.
// As candidatas devem ser populadas via SetCandidates durante o boot
// (após carregar chaves ativas do DB) e via OnKeyReenabled quando reativadas.
func NewLRUStore() domain.LRUStore {
	return &memoryLRUStore{
		byProvider:    make(map[domain.ID]map[domain.ID]time.Time),
		providerByKey: make(map[domain.ID]domain.ID),
	}
}

// SetCandidates popula as candidatas iniciais por provider.
// Deve ser chamado uma vez no boot (após carregar chaves ativas do DB) e pode
// ser chamado novamente para sincronizar (em implementation futura).
// Não é parte da interface domain.LRUStore (é um método de bootstrap específico
// desta implementação).
func (s *memoryLRUStore) SetCandidates(providerID domain.ID, keyIDs []domain.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time, len(keyIDs))
		s.byProvider[providerID] = m
	}
	for _, id := range keyIDs {
		if _, exists := m[id]; !exists {
			// Chaves novas entram como "menos usadas" (lastUsed zero → NULLS FIRST).
			// Distribui cold-start naturalmente.
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
func (s *memoryLRUStore) PickLeastUsed(_ context.Context, providerID domain.ID, exclude []domain.ID) (*domain.KeyCandidate, error) {
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

	return &domain.KeyCandidate{KeyID: bestID, ProviderID: providerID}, nil
}

// MarkUsed atualiza last_used_at sem selecionar (usado após afinidade ou sucesso).
func (s *memoryLRUStore) MarkUsed(_ context.Context, keyID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	providerID, ok := s.providerByKey[keyID]
	if !ok {
		return nil // chave não está no store; no-op
	}
	if m, ok := s.byProvider[providerID]; ok {
		if _, exists := m[keyID]; exists {
			m[keyID] = time.Now()
		}
	}
	return nil
}

// OnKeyDisabled remove a chave do pool de candidatas (auto-disable).
// Permanece em providerByKey para que OnKeyReenabled saiba o provider.
func (s *memoryLRUStore) OnKeyDisabled(_ context.Context, keyID domain.ID) error {
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
func (s *memoryLRUStore) OnKeyReenabled(_ context.Context, keyID domain.ID, providerID domain.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerByKey[keyID] = providerID
	m, ok := s.byProvider[providerID]
	if !ok {
		m = make(map[domain.ID]time.Time)
		s.byProvider[providerID] = m
	}
	m[keyID] = time.Now()
	return nil
}

// EnsureCandidate adiciona a chave ao pool SÓ se ausente, com last_used_at=zero
// (candidata "fria" a ser pickada primeiro). NÃO toca em chaves já presentes —
// preserva o ranking LRU. Usa-se em cada request para incluir chaves cadastradas
// após o boot sem corromper o LRU (bug anterior: OnKeyReenabled resetava tudo).
func (s *memoryLRUStore) EnsureCandidate(_ context.Context, keyID domain.ID, providerID domain.ID) error {
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
