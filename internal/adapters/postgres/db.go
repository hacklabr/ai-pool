package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hacklabr/ai-pool/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool cria e configura um pool de conexões pgxpool com defaults sensatos.
//
// O pool é thread-safe e deve ser reutilizado por toda a aplicação (singleton).
// Em shutdown, chamar Close() para liberar conexões (drenagem graceful).
//
// Defaults (spec §3.4.5, DevOps §5.2):
//   - max_conns:    DB_MAX_CONNS (default 60) — suporta múltiplos pods × usuários simultâneos.
//   - min_conns:    DB_MIN_CONNS (default 5)  — evita cold-start de conexão.
//   - max_lifetime: DB_CONN_MAX_LIFETIME (default 1h) — força reestabelecimento
//     para coletar novas configurações do servidor e evitar stale conns.
//   - health_check_period: 30s — detecta conexões mortas (failover de DB).
//
// A validação da DSN (incluindo sslmode) é responsabilidade do caller; em modo
// produção (DevMode=false) o config.Validate() já exige DBDSN não-vazio.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	if cfg == nil {
		return nil, errors.New("postgres.NewPool: nil config")
	}
	if cfg.DBDSN == "" {
		return nil, errors.New("postgres.NewPool: DBDSN vazio (configure DATABASE_URL)")
	}

	pcfg, err := pgxpool.ParseConfig(cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("postgres.NewPool: DSN inválida: %w", err)
	}

	// Calibragem do pool.
	if cfg.DBMaxConns > 0 {
		pcfg.MaxConns = cfg.DBMaxConns
	}
	if cfg.DBMinConns >= 0 {
		pcfg.MinConns = cfg.DBMinConns
	}
	if cfg.DBConnMaxLifetime > 0 {
		pcfg.MaxConnLifetime = cfg.DBConnMaxLifetime
	}
	// Health check detecta conexões quebradas (ex.: failover de managed DB).
	pcfg.HealthCheckPeriod = 30 * time.Second
	// Idle timeout evita manter conexões abertas sem uso.
	pcfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("postgres.NewPool: %w", err)
	}

	// Ping inicial confirma que o DB está alcançável e as credenciais estão OK.
	// Falha aqui impede o pod de ficar "pronto" (readinessProbe em /readyz
	// depende disto para retirar o pod do Service até o DB voltar).
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres.NewPool: ping falhou: %w", err)
	}

	return pool, nil
}
