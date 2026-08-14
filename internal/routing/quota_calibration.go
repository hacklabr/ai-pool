package routing

import (
	"context"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
)

// =============================================================================
// Porta mínima de calibração empírica de cota (issue #20, RF-05).
//
// Quando um auto-disable classifica o erro como teto de uma janela do plano
// (window_5h/weekly/monthly), a soma de tokens da janela é persistida como
// MEDIÇÃO OBSERVADA da cota (piso conservador) em key_quota_measurements.
// Ao longo dos ciclos, o MIN das medições refina o denominador cadastrado
// (provider_keys.plan_quota_tokens).
//
// O routing só informa o EVENTO (chave, camada, instante, mensagem
// sanitizada); o adapter (postgres.QuotaCalibrationAdapter) calcula a
// janela, soma os tokens de usage_logs e persiste. Regra do repo: "nova
// dependência externa = porta mínima no pacote consumidor".
//
// Best-effort: falha ao registrar a medição NUNCA pode derrubar o
// disable/audit já persistido (implementações devem ser tolerantes).
//
// NOTA: tokens ≠ unidade interna do provedor (créditos Z.ai com
// multiplicadores/pico; 额度 Kimi) — a medição é piso conservador para o
// mix de uso observado.
// =============================================================================

// QuotaMeasurementRecorder é a porta mínima de registro de medições de cota.
type QuotaMeasurementRecorder interface {
	// RecordCeilingHit registra a medição da cota da camada `layer` para a
	// chave, com a janela computada a partir de disabledAt (5h fixa para
	// window_5h; ciclos ancorados em plan_renewal_date para weekly/monthly).
	// sanitizedMessage já passou por SanitizeUpstreamMessage (sem segredos).
	RecordCeilingHit(ctx context.Context, orgID domain.OrganizationID, keyID domain.ID, layer string, disabledAt time.Time, sanitizedMessage string) error
}
