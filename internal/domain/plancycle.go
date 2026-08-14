package domain

import (
	"math"
	"time"
)

// =============================================================================
// Ciclo do plano de assinatura (issue #20, RF-02/RF-04/RF-05).
//
// Os planos usados pelo pool (Z.ai GLM Coding Plan, Kimi Code/Membership)
// têm cotas em janelas (5h / semanal / mensal) ancoradas na DATA DE
// RENOVAÇÃO da assinatura, e nenhum provedor expõe API de cota/consumo.
// O cálculo do ciclo é feito localmente a partir dessa âncora.
//
// IMPORTANTE — unidade de medida: tokens ≠ unidade interna do provedor
// (créditos do Z.ai têm multiplicadores e bônus de pico; o Kimi mede em
// 额度). A soma local de tokens em usage_logs é um PISO CONSERVADOR do
// consumo real para o mix de uso observado — suficiente para comparar
// contra a cota calibrada empiricamente (RF-05).
//
// Funções PURAS (sem I/O, sem estado) — testáveis isoladamente
// (ver plancycle_test.go).
// =============================================================================

// Confiança do denominador da cota (issue #20, RF-05).
const (
	// QuotaConfidenceCalibrated: piso empírico observado em desativações
	// anteriores (MIN das medições da camada) — melhor estimativa.
	QuotaConfidenceCalibrated = "calibrated"
	// QuotaConfidenceConfigured: valor cadastrado pelo admin
	// (provider_keys.plan_quota_tokens).
	QuotaConfidenceConfigured = "configured"
	// QuotaConfidenceNone: sem denominador — % não é exibida (UI pede para
	// cadastrar a cota do plano).
	QuotaConfidenceNone = "none"
)

// lastDayOfMonth retorna o último dia do mês (28/29/30/31).
func lastDayOfMonth(year int, month time.Month) int {
	// Dia 0 do mês seguinte = último dia do mês corrente.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// addMonthsYM soma meses a um par (ano, mês) sem criar datas inválidas
// (ex.: jan + 1 mês = fev do mesmo ano; dez + 2 = fev do ano seguinte).
func addMonthsYM(year int, month time.Month, add int) (int, time.Month) {
	t := time.Date(year, month+time.Month(add), 1, 0, 0, 0, 0, time.UTC)
	return t.Year(), t.Month()
}

// anniversaryInMonth projeta o dia-da-mês da âncora em outro mês/ano,
// com CLAMP para meses sem o dia (ex.: dia 31 em abril → 30; dia 29/31 em
// fevereiro → 28/29). Preserva a hora da âncora e o fuso (Location).
func anniversaryInMonth(anchor time.Time, year int, month time.Month) time.Time {
	day := anchor.Day()
	if last := lastDayOfMonth(year, month); day > last {
		day = last
	}
	return time.Date(year, month, day,
		anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(),
		anchor.Location())
}

// MonthlyCycleWindow retorna a janela [start, end) do ciclo MENSAL corrente
// em relação a `now`, ancorada no dia-da-mês de planRenewalDate (a data de
// renovação da assinatura). O fim do ciclo é o próximo aniversário da
// âncora (com clamp de dia em meses curtos).
//
// Fallback para data de renovação futura (assinatura recém-cadastrada ou
// data errada): a própria data de renovação é tratada como início da
// âncora — [renewalDate, próximo aniversário). `now` antes do início
// implica uso zero no ciclo, o que é seguro (superestima o % restante,
// nunca subestima o consumo).
//
// planRenewalDate zero retorna janelas zero (caller deve guardar).
func MonthlyCycleWindow(planRenewalDate, now time.Time) (start, end time.Time) {
	if planRenewalDate.IsZero() {
		return time.Time{}, time.Time{}
	}
	months := (now.Year()-planRenewalDate.Year())*12 + int(now.Month()) - int(planRenewalDate.Month())
	if months < 0 {
		months = 0 // renovação futura: primeiro ciclo começa na própria âncora
	}
	y, m := addMonthsYM(planRenewalDate.Year(), planRenewalDate.Month(), months)
	start = anniversaryInMonth(planRenewalDate, y, m)
	if start.After(now) && months > 0 {
		// O aniversário projetado caiu depois de now (ex.: âncora dia 31,
		// mês corrente só tem 30) → o ciclo corrente é o anterior.
		months--
		y, m = addMonthsYM(planRenewalDate.Year(), planRenewalDate.Month(), months)
		start = anniversaryInMonth(planRenewalDate, y, m)
	}
	ey, em := addMonthsYM(planRenewalDate.Year(), planRenewalDate.Month(), months+1)
	end = anniversaryInMonth(planRenewalDate, ey, em)
	return start, end
}

// WeeklyCycleWindow retorna a janela [start, end) do ciclo SEMANAL corrente:
// janelas de 7 dias de calendário ancoradas em planRenewalDate
// (start = âncora + k*7d, com k o maior inteiro tal que start <= now).
//
// planRenewalDate zero retorna janelas zero (caller deve guardar).
func WeeklyCycleWindow(planRenewalDate, now time.Time) (start, end time.Time) {
	if planRenewalDate.IsZero() {
		return time.Time{}, time.Time{}
	}
	start = planRenewalDate
	if !planRenewalDate.After(now) {
		days := int(math.Floor(now.Sub(planRenewalDate).Hours() / 24))
		start = planRenewalDate.AddDate(0, 0, (days/7)*7)
		if start.After(now) {
			start = start.AddDate(0, 0, -7)
		}
	}
	// Renovação futura: ciclo começa na própria âncora (mesmo fallback do
	// mensal — uso zero até a âncora).
	return start, start.AddDate(0, 0, 7)
}

// DaysUntilCycleEnd retorna os dias (arredondados PARA CIMA) entre now e o
// fim do ciclo. cycleEnd <= now → 0.
func DaysUntilCycleEnd(cycleEnd, now time.Time) int {
	d := cycleEnd.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Hours() / 24))
}

// EffectiveQuota é o denominador efetivo da cota de uma camada, com a
// confiança da estimativa (issue #20, RF-05).
type EffectiveQuota struct {
	Denominator int64  // 0 quando Confidence == none
	Confidence  string // QuotaConfidence{Calibrated,Configured,None}
}

// ResolveEffectiveQuota calcula o denominador efetivo para uma camada:
//
//   - measuredFloor != nil (≥1 medição observada na camada): piso
//     conservador = MIN das medições, confidence=calibrated. O MIN evita
//     superestimar a cota (uma desativação precoce seria invisível com MAX).
//   - senão, planQuotaTokens > 0: valor cadastrado pelo admin,
//     confidence=configured.
//   - senão: confidence=none — sem % no painel (UI pede para cadastrar).
func ResolveEffectiveQuota(measuredFloor *int64, planQuotaTokens *int64) EffectiveQuota {
	if measuredFloor != nil && *measuredFloor > 0 {
		return EffectiveQuota{Denominator: *measuredFloor, Confidence: QuotaConfidenceCalibrated}
	}
	if planQuotaTokens != nil && *planQuotaTokens > 0 {
		return EffectiveQuota{Denominator: *planQuotaTokens, Confidence: QuotaConfidenceConfigured}
	}
	return EffectiveQuota{Confidence: QuotaConfidenceNone}
}

// IsUnderutilized implementa a regra de subutilização (issue #20, RF-04):
//
//	planConfigured (data de renovação E denominador disponível — calibrado
//	ou cadastrado) E dias até o fim do ciclo mensal <= thresholdDays E
//	uso mensal (%) < thresholdPct.
//
// Chaves sem denominador (confidence=none) NUNCA são subutilizadas — sem
// cota não há como distinguir subutilização de "plano não medido".
func IsUnderutilized(planConfigured bool, monthlyUsagePct float64, daysUntilCycleEnd, thresholdDays int, thresholdPct float64) bool {
	return planConfigured &&
		daysUntilCycleEnd <= thresholdDays &&
		monthlyUsagePct < thresholdPct
}
