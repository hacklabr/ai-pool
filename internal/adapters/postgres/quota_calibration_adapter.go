package postgres

import (
	"context"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/internal/routing"
	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// QuotaCalibrationAdapter — adapter da porta routing.QuotaMeasurementRecorder
// (issue #20, RF-05). Chamado pelo AutoDisableManager quando uma
// desativação é classificada como teto de janela do plano:
//
//   - window_5h → janela fixa [disabledAt-5h, disabledAt);
//   - weekly/monthly → janela do ciclo ancorada em provider_keys.
//     plan_renewal_date (domain.WeeklyCycleWindow/MonthlyCycleWindow);
//     sem data de renovação não há janela calculável → skip intencional
//     (retorna nil, sem erro — não é falha).
//
// Soma os tokens da chave na janela (UMA query em usage_logs via índice
// idx_usage_logs_key_time) e persiste em key_quota_measurements com a
// mensagem upstream sanitizada (≤200 chars, sem segredos).
//
// NOTA: tokens ≠ unidade interna do provedor (créditos Z.ai com
// multiplicadores e bônus de pico; 额度 Kimi). A medição é um PISO
// CONSERVADOR da cota real para o mix de uso observado — por isso o
// denominador calibrado usa o MIN entre medições.
//
// Best-effort: o erro é devolvido ao caller (routing loga em warn), mas
// NUNCA deve derrubar o caminho de disable/audit.
// =============================================================================

// Compile-time assertion: satisfaz a porta do routing.
var _ routing.QuotaMeasurementRecorder = (*QuotaCalibrationAdapter)(nil)

// QuotaCalibrationAdapter adapta UsageLogRepo + KeyQuotaMeasurementRepo à
// porta routing.QuotaMeasurementRecorder.
type QuotaCalibrationAdapter struct {
	pool      *pgxpool.Pool
	usageRepo *UsageLogRepo
	measRepo  *KeyQuotaMeasurementRepo
}

// NewQuotaCalibrationAdapter monta o adapter.
func NewQuotaCalibrationAdapter(pool *pgxpool.Pool) *QuotaCalibrationAdapter {
	return &QuotaCalibrationAdapter{
		pool:      pool,
		usageRepo: NewUsageLogRepo(pool),
		measRepo:  NewKeyQuotaMeasurementRepo(pool),
	}
}

// RecordCeilingHit implementa routing.QuotaMeasurementRecorder. Ver comentário
// do arquivo para a semântica da janela por camada.
func (a *QuotaCalibrationAdapter) RecordCeilingHit(
	ctx context.Context,
	orgID domain.OrganizationID,
	keyID domain.ID,
	layer string,
	disabledAt time.Time,
	sanitizedMessage string,
) error {
	var start, end time.Time
	switch layer {
	case routing.LimitLayerWindow5h:
		start, end = disabledAt.Add(-5*time.Hour), disabledAt
	case routing.LimitLayerWeekly, routing.LimitLayerMonthly:
		renewal, ok, err := a.keyPlanRenewalDate(ctx, int64(orgID), int64(keyID))
		if err != nil {
			return err
		}
		if !ok {
			// Chave sem âncora de plano (pay-as-you-go ou campo vazio):
			// não há ciclo calculável — skip intencional, não é erro.
			return nil
		}
		if layer == routing.LimitLayerWeekly {
			start, end = domain.WeeklyCycleWindow(renewal, disabledAt)
		} else {
			start, end = domain.MonthlyCycleWindow(renewal, disabledAt)
		}
	default:
		// Camadas que não são teto de janela (balance/tier/other) não geram
		// medição — o caller do routing já filtra; defesa em profundidade.
		return nil
	}

	usage, err := a.usageRepo.QueryKeyUsageInWindow(ctx, int64(orgID), []int64{int64(keyID)}, start, end)
	if err != nil {
		return err
	}
	var tokens int64
	if u, ok := usage[int64(keyID)]; ok {
		tokens = u.TotalTokens
	}

	var msg *string
	if s := truncateText(sanitizedMessage, 200); s != "" {
		msg = &s
	}
	_, err = a.measRepo.Create(ctx, &KeyQuotaMeasurement{
		OrganizationID: int64(orgID),
		KeyID:          int64(keyID),
		LimitLayer:     layer,
		Tokens:         tokens,
		WindowStart:    start.UTC(),
		WindowEnd:      end.UTC(),
		ErrorMessage:   msg,
	})
	return err
}

// keyPlanRenewalDate carrega a data de renovação do plano da chave
// (âncora dos ciclos). ok=false quando o campo é NULL ou a chave não existe.
func (a *QuotaCalibrationAdapter) keyPlanRenewalDate(ctx context.Context, orgID, keyID int64) (time.Time, bool, error) {
	const q = `SELECT plan_renewal_date
	FROM provider_keys
	WHERE organization_id = $1 AND id = $2`
	var renewal *time.Time
	if err := a.pool.QueryRow(ctx, q, orgID, keyID).Scan(&renewal); err != nil {
		if isNoRows(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	if renewal == nil {
		return time.Time{}, false, nil
	}
	return renewal.UTC(), true, nil
}

// truncateText corta a string em até max BYTES, recuando até uma fronteira
// válida de rune UTF-8 (defesa em profundidade — SanitizeUpstreamMessage já
// limita a saída em 200; a convenção da coluna é "≤200 chars").
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Corta no último byte < max que inicia um rune válido.
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
