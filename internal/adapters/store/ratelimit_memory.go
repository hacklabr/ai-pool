package store

import (
	"context"
	"sync"
	"time"

	"github.com/rafael/ai-pool/internal/domain"
	"golang.org/x/time/rate"
)

// memoryRateLimiter implementa domain.RateLimiter via golang.org/x/time/rate
// (token bucket). In-memory, por escopo ( userID ou token keyID).
//
// Em restart perde o estado (aceitável no MVP, 1 réplica). Migração futura
// para RedisStore via GCRA ou sliding window (spec §3.2.5).
//
// Defaults racionais para 30 usuários internos:
//   - burst (capacidade): 10
//   - ratePerMinute: 60 (1 req/s sustentado, com burst inicial de 10)
type memoryRateLimiter struct {
	mu              sync.Mutex
	limiters        map[string]*rate.Limiter // scope -> limiter
	defaultBurst    int
	defaultRatePerM int
	now             func() time.Time
}

// RateLimiterOption configura o construtor.
type RateLimiterOption func(*memoryRateLimiter)

// WithRateLimitDefaults sobrescreve burst e rate default (req/min).
func WithRateLimitDefaults(burst, ratePerMinute int) RateLimiterOption {
	return func(r *memoryRateLimiter) {
		r.defaultBurst = burst
		r.defaultRatePerM = ratePerMinute
	}
}

// NewRateLimiter cria um RateLimiter in-memory.
func NewRateLimiter(opts ...RateLimiterOption) domain.RateLimiter {
	r := &memoryRateLimiter{
		limiters:        make(map[string]*rate.Limiter),
		defaultBurst:    10,
		defaultRatePerM: 60,
		now:             time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Allow consome 1 token do bucket. Cria o bucket sob demanda com defaults se
// o escopo ainda não existe (a configuração específica via SetLimit prevalece
// se chamada anteriormente).
//
// retryAfter é estimado a partir da próxima permissão disponível do rate.Limiter
// (rate.Limiter.Reserve().Delay()).
func (r *memoryRateLimiter) Allow(_ context.Context, scope string) (bool, time.Duration, error) {
	lim := r.getOrCreate(scope)
	res := lim.Reserve()
	if !res.OK() {
		// Limite zero (infinitamente bloqueado). Não deveria ocorrer com defaults.
		return false, time.Hour, nil
	}
	delay := res.Delay()
	if delay <= 0 {
		return true, 0, nil
	}
	// Cancela a reserva para não consumir token não usado (devolve ao bucket).
	res.Cancel()
	return false, delay, nil
}

// SetLimit configura capacidade e taxa por escopo. Aplica-se imediatamente.
// Substitui o limiter existente para o escopo.
func (r *memoryRateLimiter) SetLimit(_ context.Context, scope string, burst, ratePerMinute int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// rate.NewLimiter recebe rate por SEGUNDO. Convertemos req/min → req/s.
	perSecond := rate.Limit(ratePerMinute) / rate.Limit(60)
	if perSecond <= 0 {
		perSecond = rate.Every(time.Hour) // ~zero: efetivamente bloqueado
	}
	r.limiters[scope] = rate.NewLimiter(perSecond, burst)
	return nil
}

// getOrCreate retorna o limiter do escopo, criando com defaults se ausente.
func (r *memoryRateLimiter) getOrCreate(scope string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	lim, ok := r.limiters[scope]
	if !ok {
		perSecond := rate.Limit(r.defaultRatePerM) / rate.Limit(60)
		lim = rate.NewLimiter(perSecond, r.defaultBurst)
		r.limiters[scope] = lim
	}
	return lim
}
