package routing

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
	"github.com/rafael/ai-pool/internal/observability"
)

// =============================================================================
// AutoDisableManager — contador de erros por chave em janela deslizante +
// threshold + transição para auto_disabled.
//
// Spec §3.2.4:
//   - Gatilho: N erros em janela de tempo → desabilitada.
//   - Parse da mensagem de erro → data de reativação.
//   - Se parsing falha → disabled_until = agora + backoff crescente.
//   - Persistência: disabled_until em PostgreSQL (write-through CAS atômico).
//   - Estado interno (contadores) em memória: estado em janela não precisa ser
//     persistido (perder em restart = conta do zero, aceitável).
//
// Decisões F5:
//   - Threshold: 5 erros em janela de 2min (configurável).
//   - Backoff crescente se parse falha: [1min, 5min, 15min, 1h, 6h, 24h] por
//     ciclo de desabilitação consecutiva (consecutiveDisableCount).
//   - Reset do contador em sucesso (OnKeySucceeded chama ResetCounter).
// =============================================================================

// AutoDisableConfig parametriza o AutoDisableManager.
type AutoDisableConfig struct {
	Threshold int           // default 5
	Window    time.Duration // default 2min
	// BackoffSteps应用于consecutivas desabilitações SEM data parseada.
	// Índice = consecutiveDisableCount % len(steps).
	BackoffSteps []time.Duration
}

// DefaultAutoDisableConfig retorna defaults da spec.
func DefaultAutoDisableConfig() AutoDisableConfig {
	return AutoDisableConfig{
		Threshold: 5,
		Window:    2 * time.Minute,
		BackoffSteps: []time.Duration{
			1 * time.Minute,
			5 * time.Minute,
			15 * time.Minute,
			1 * time.Hour,
			6 * time.Hour,
			24 * time.Hour,
		},
	}
}

// keyStateKey identifica uma chave no contexto do manager (orgID+keyID).
// multi-tenant ready.
type keyStateKey struct {
	OrgID int64
	KeyID int64
}

// keyState é o contador de erros em janela por chave.
type keyState struct {
	mu                      sync.Mutex
	errorCount              int       // total na janela atual
	windowStart             time.Time // início da janela deslizante
	consecutiveDisableCount int       // quantas vezes foi auto-desabilitada seguida
}

// AutoDisableManager gerencia contadores de erro e dispara transições.
//
// Thread-safe (sync.RWMutex no map de estados).
type AutoDisableManager struct {
	cfg      AutoDisableConfig
	parsers  *ParserRegistry
	keyRepo  KeyRepoPort
	lruStore domain.LRUStore
	logger   *slog.Logger
	now      func() time.Time

	mu     sync.RWMutex
	states map[keyStateKey]*keyState
}

// KeyRepoPort é a porta mínima do KeyRepo necessária ao manager.
// Permite mock em testes sem depender de PostgreSQL.
type KeyRepoPort interface {
	TransitionToAutoDisabled(
		ctx context.Context,
		orgID domain.OrganizationID,
		keyID domain.ID,
		reason domain.KeyDisabledReason,
		disabledUntil time.Time,
		errorCount int,
		consecutiveErrors int,
		lastErrorHint *string,
	) error
}

// NewAutoDisableManager cria o manager.
func NewAutoDisableManager(
	cfg AutoDisableConfig,
	parsers *ParserRegistry,
	keyRepo KeyRepoPort,
	lruStore domain.LRUStore,
	logger *slog.Logger,
) *AutoDisableManager {
	return &AutoDisableManager{
		cfg:      cfg,
		parsers:  parsers,
		keyRepo:  keyRepo,
		lruStore: lruStore,
		logger:   logger,
		now:      time.Now,
		states:   make(map[keyStateKey]*keyState),
	}
}

// RecordError registra um erro para a chave e dispara auto-disable se atingir
// threshold.
//
// Cenários:
//   - Erro dentro da janela atual → incrementa contador.
//   - Erro FORA da janela (expirou) → reseta janela e conta 1.
//   - Contador atinge threshold → chama parser; se achar data, usa; senão
//     usa backoff crescente; persiste via KeyRepo.TransitionToAutoDisabled
//     (write-through CAS); notifica LRUStore.OnKeyDisabled; incrementa
//     consecutiveDisableCount.
//
// Após disable, o contador da janela é RESETADO (a chave está desabilitada,
// não receberá mais erros até ser reativada).
func (m *AutoDisableManager) RecordError(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	providerID domain.ID,
	providerSlug string,
	statusCode int,
	errBody []byte,
) error {
	now := m.now()
	k := keyStateKey{OrgID: int64(orgID), KeyID: int64(keyID)}

	// Pega/cria state.
	m.mu.Lock()
	st, ok := m.states[k]
	if !ok {
		st = &keyState{windowStart: now}
		m.states[k] = st
	}
	m.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()

	// Janela expirou? Reseta.
	if now.Sub(st.windowStart) > m.cfg.Window {
		st.windowStart = now
		st.errorCount = 0
	}
	st.errorCount++

	// Ainda abaixo do threshold: apenas contabiliza.
	if st.errorCount < m.cfg.Threshold {
		m.logger.Debug("auto-disable: error recorded",
			"key_id", keyID,
			"errors_in_window", st.errorCount,
			"threshold", m.cfg.Threshold,
		)
		return nil
	}

	// Atingiu threshold → desabilitar.
	// 1. Tenta parsear data de reativação da mensagem de erro.
	resetAt, parseOK := m.parsers.ParseResetDate(providerSlug, statusCode, errBody)

	var (
		disabledUntil time.Time
		reason        domain.KeyDisabledReason
		hint          *string
	)
	if parseOK && resetAt.After(now) {
		// Parser achou data futura → usa ela.
		disabledUntil = resetAt
		reason = domain.ReasonQuotaExceeded
		// Hint legível (não logar o body cru, só a data detectada).
		h := resetAt.UTC().Format(time.RFC3339)
		hint = &h
		m.logger.Info("auto-disable: parsed reset date",
			"key_id", keyID,
			"reset_at", disabledUntil,
			"reason", string(reason),
		)
	} else {
		// Parser falhou → backoff crescente.
		idx := st.consecutiveDisableCount % len(m.cfg.BackoffSteps)
		disabledUntil = now.Add(m.cfg.BackoffSteps[idx])
		reason = domain.ReasonParseFailedRetry
		m.logger.Info("auto-disable: parse failed, using growing backoff",
			"key_id", keyID,
			"disabled_until", disabledUntil,
			"backoff_step", m.cfg.BackoffSteps[idx],
			"consecutive_disables", st.consecutiveDisableCount,
		)
	}

	// 2. Write-through CAS atômico no DB. Se CAS perdido (outro worker já
	// desabilitou), não é erro — apenas loga e segue.
	if err := m.keyRepo.TransitionToAutoDisabled(
		ctx, orgID, keyID, reason, disabledUntil,
		st.errorCount, st.errorCount, hint,
	); err != nil {
		m.logger.Warn("auto-disable: TransitionToAutoDisabled failed",
			"key_id", keyID, "err", err)
		// Não propagamos — o erro já foi registrado; próxima chamada pode
		// tentar de novo. Continuidade do pool não é prejudicada.
	} else {
		// CAS venceu (transição aplicada): registra métrica.
		// Observability é importado em auto_disable_test.go; o import real
		// vive neste arquivo (ver import block no topo).
		observability.IncKeyDisabled("auto")
	}

	// 3. Notifica LRUStore (remove das candidatas).
	if m.lruStore != nil {
		if err := m.lruStore.OnKeyDisabled(ctx, keyID); err != nil {
			m.logger.Warn("auto-disable: LRUStore.OnKeyDisabled failed", "err", err)
		}
	}

	// 4. Atualiza state interno: incrementa consecutive, reseta janela.
	st.consecutiveDisableCount++
	st.errorCount = 0
	st.windowStart = now // reseta janela

	return nil
}

// ResetCounter reseta contadores da chave (usado em OnKeySucceeded).
// Sucesso significa "chave está funcionando" — contador consecutivo deve voltar a 0.
func (m *AutoDisableManager) ResetCounter(orgID domain.OrganizationID, keyID domain.ID) {
	k := keyStateKey{OrgID: int64(orgID), KeyID: int64(keyID)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.states[k]; ok {
		st.mu.Lock()
		st.errorCount = 0
		st.consecutiveDisableCount = 0 // sucesso zera a contagem de disables consecutivos
		st.windowStart = m.now()
		st.mu.Unlock()
	}
}
