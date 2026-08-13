package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hacklabr/ai-pool/internal/config"
)

// TestRunSyncModels_ExitCodes cobre os exit codes do subcomando --sync-models
// que não exigem DB real (RF-11):
//   - DATABASE_URL ausente → 1 (falha estrutural).
//
// Os caminhos "exit 0 sem providers/chaves" e "exit 0 com provedor falhando
// (fail-safe)" são cobertos pelos testes do pacote modelsync (SyncAll não
// retorna erro nesses casos — ver modelsync_test.go), pois exigem DB/upstream.
func TestRunSyncModels_ExitCodes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("missing DATABASE_URL returns 1", func(t *testing.T) {
		cfg := &config.Config{DBDSN: ""}
		if code := runSyncModels(cfg, logger); code != 1 {
			t.Errorf("runSyncModels(DBDSN vazio) = %d, want 1", code)
		}
	})

	t.Run("unreachable DB returns 1", func(t *testing.T) {
		// DSN apontando para porta fechada: NewPool falha (falha estrutural).
		cfg := &config.Config{
			DBDSN:             "postgres://aipp:aipp@127.0.0.1:1/aipp?connect_timeout=1",
			DBMaxConns:        1,
			DBMinConns:        0,
			ModelSyncInterval: 0,
		}
		if code := runSyncModels(cfg, logger); code != 1 {
			t.Errorf("runSyncModels(DB inalcançável) = %d, want 1", code)
		}
	})
}
