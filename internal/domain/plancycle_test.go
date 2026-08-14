package domain

import (
	"testing"
	"time"
)

// =============================================================================
// Ciclo do plano (issue #20, RF-02): janelas mensal/semanal ancoradas na
// data de renovação. Casos de borda: clamp de dia 31, virada de ano, ano
// bissexto, renovação futura, fronteira exata da janela.
// =============================================================================

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func TestMonthlyCycleWindow_MidCycle(t *testing.T) {
	// Âncora dia 15; agora dia 20 do mês seguinte → ciclo [15, próximo 15).
	start, end := MonthlyCycleWindow(d(2026, 1, 15), d(2026, 2, 20))
	if !start.Equal(d(2026, 2, 15)) {
		t.Errorf("start = %v, want 2026-02-15", start)
	}
	if !end.Equal(d(2026, 3, 15)) {
		t.Errorf("end = %v, want 2026-03-15", end)
	}
}

func TestMonthlyCycleWindow_ExactAnniversary(t *testing.T) {
	// Agora É o aniversário → novo ciclo começa hoje (janela [start,end)).
	start, end := MonthlyCycleWindow(d(2026, 1, 15), d(2026, 2, 15))
	if !start.Equal(d(2026, 2, 15)) || !end.Equal(d(2026, 3, 15)) {
		t.Errorf("janela = [%v, %v), want [2026-02-15, 2026-03-15)", start, end)
	}
}

func TestMonthlyCycleWindow_SameMonthBeforeAnchorDay(t *testing.T) {
	// Renovou dia 15 deste mês; hoje é dia 10 → ciclo corrente é o
	// anterior: [15 do mês passado, 15 deste mês).
	start, end := MonthlyCycleWindow(d(2026, 1, 15), d(2026, 2, 10))
	if !start.Equal(d(2026, 1, 15)) || !end.Equal(d(2026, 2, 15)) {
		t.Errorf("janela = [%v, %v), want [2026-01-15, 2026-02-15)", start, end)
	}
}

func TestMonthlyCycleWindow_Day31Clamp(t *testing.T) {
	// Âncora dia 31; meses de 30 dias clampeiam para o último dia.
	start, end := MonthlyCycleWindow(d(2026, 1, 31), d(2026, 3, 10))
	if !start.Equal(d(2026, 2, 28)) { // fev não-bissexto → 28
		t.Errorf("start = %v, want 2026-02-28 (clamp de dia 31)", start)
	}
	if !end.Equal(d(2026, 3, 31)) { // mar tem 31 → volta ao dia original
		t.Errorf("end = %v, want 2026-03-31", end)
	}
}

func TestMonthlyCycleWindow_LeapYear(t *testing.T) {
	// Âncora 29/02 em ano bissexto; ano seguinte não-bissexto → clamp 28.
	start, end := MonthlyCycleWindow(d(2024, 2, 29), d(2026, 2, 28))
	if !start.Equal(d(2026, 2, 28)) {
		t.Errorf("start = %v, want 2026-02-28", start)
	}
	if !end.Equal(d(2026, 3, 29)) {
		t.Errorf("end = %v, want 2026-03-29", end)
	}
}

func TestMonthlyCycleWindow_YearRollover(t *testing.T) {
	// Âncora dez; agora jan → ciclo atravessa a virada do ano.
	start, end := MonthlyCycleWindow(d(2025, 12, 15), d(2026, 1, 3))
	if !start.Equal(d(2025, 12, 15)) || !end.Equal(d(2026, 1, 15)) {
		t.Errorf("janela = [%v, %v), want [2025-12-15, 2026-01-15)", start, end)
	}
}

func TestMonthlyCycleWindow_FutureRenewal(t *testing.T) {
	// Renovação futura: a própria data é o início da âncora (fallback).
	start, end := MonthlyCycleWindow(d(2026, 9, 10), d(2026, 8, 14))
	if !start.Equal(d(2026, 9, 10)) {
		t.Errorf("start = %v, want 2026-09-10 (âncora futura = início)", start)
	}
	if !end.Equal(d(2026, 10, 10)) {
		t.Errorf("end = %v, want 2026-10-10", end)
	}
}

func TestMonthlyCycleWindow_ZeroAnchor(t *testing.T) {
	start, end := MonthlyCycleWindow(time.Time{}, time.Now())
	if !start.IsZero() || !end.IsZero() {
		t.Errorf("âncora zero deve retornar janelas zero, got [%v, %v)", start, end)
	}
}

func TestWeeklyCycleWindow_MidWindow(t *testing.T) {
	// Âncora dia 1; dia 10 = 9 dias depois → semana corrente [8, 15).
	start, end := WeeklyCycleWindow(d(2026, 8, 1), d(2026, 8, 10))
	if !start.Equal(d(2026, 8, 8)) || !end.Equal(d(2026, 8, 15)) {
		t.Errorf("janela = [%v, %v), want [2026-08-08, 2026-08-15)", start, end)
	}
}

func TestWeeklyCycleWindow_ExactBoundary(t *testing.T) {
	// Agora É o início de uma nova janela (k*7d exato) → a nova janela vale.
	start, _ := WeeklyCycleWindow(d(2026, 8, 1), d(2026, 8, 8))
	if !start.Equal(d(2026, 8, 8)) {
		t.Errorf("start = %v, want 2026-08-08 (fronteira exata)", start)
	}
}

func TestWeeklyCycleWindow_JustBeforeBoundary(t *testing.T) {
	// Uma hora antes da fronteira → janela anterior.
	now := d(2026, 8, 8).Add(-time.Hour)
	start, _ := WeeklyCycleWindow(d(2026, 8, 1), now)
	if !start.Equal(d(2026, 8, 1)) {
		t.Errorf("start = %v, want 2026-08-01", start)
	}
}

func TestWeeklyCycleWindow_FutureRenewal(t *testing.T) {
	start, end := WeeklyCycleWindow(d(2026, 8, 20), d(2026, 8, 14))
	if !start.Equal(d(2026, 8, 20)) || !end.Equal(d(2026, 8, 27)) {
		t.Errorf("janela = [%v, %v), want [2026-08-20, 2026-08-27)", start, end)
	}
}

func TestDaysUntilCycleEnd(t *testing.T) {
	now := d(2026, 8, 14)
	cases := []struct {
		end  time.Time
		want int
	}{
		{now.Add(2 * 24 * time.Hour), 2},
		{now.Add(6 * time.Hour), 1},  // arredonda para cima
		{now.Add(-time.Hour), 0},     // já passou
		{now, 0},                     // exatamente agora
		{now.Add(36 * time.Hour), 2}, // 1,5 dia → 2
	}
	for _, c := range cases {
		if got := DaysUntilCycleEnd(c.end, now); got != c.want {
			t.Errorf("DaysUntilCycleEnd(%v) = %d, want %d", c.end, got, c.want)
		}
	}
}

// =============================================================================
// Denominador efetivo e subutilização (issue #20, RF-04/RF-05).
// =============================================================================

func i64ptr(v int64) *int64 { return &v }

func TestResolveEffectiveQuota(t *testing.T) {
	cases := []struct {
		name      string
		measured  *int64
		planQuota *int64
		wantDenom int64
		wantConf  string
	}{
		{"calibrada vence cadastrada", i64ptr(900), i64ptr(1200), 900, QuotaConfidenceCalibrated},
		{"calibrada sem cota cadastrada", i64ptr(500), nil, 500, QuotaConfidenceCalibrated},
		{"medição zero é ignorada (piso inválido)", i64ptr(0), i64ptr(1200), 1200, QuotaConfidenceConfigured},
		{"cadastrada", nil, i64ptr(1200), 1200, QuotaConfidenceConfigured},
		{"cota cadastrada zero é ignorada", nil, i64ptr(0), 0, QuotaConfidenceNone},
		{"sem nada", nil, nil, 0, QuotaConfidenceNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveEffectiveQuota(c.measured, c.planQuota)
			if got.Denominator != c.wantDenom || got.Confidence != c.wantConf {
				t.Errorf("ResolveEffectiveQuota(%v, %v) = %+v, want {denom=%d conf=%s}",
					c.measured, c.planQuota, got, c.wantDenom, c.wantConf)
			}
		})
	}
}

func TestIsUnderutilized(t *testing.T) {
	const pct, days = 30.0, 2
	cases := []struct {
		name       string
		planConfig bool
		usagePct   float64
		daysToEnd  int
		want       bool
	}{
		{"regra completa", true, 10, 1, true},
		{"uso acima do threshold", true, 50, 1, false},
		{"ciclo longe do fim", true, 10, 10, false},
		{"sem plano configurado", false, 0, 1, false},
		{"exatamente no threshold de dias", true, 10, 2, true},
		{"exatamente no threshold de pct (não é menor)", true, 30, 1, false},
		{"uso zero", true, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUnderutilized(c.planConfig, c.usagePct, c.daysToEnd, days, pct); got != c.want {
				t.Errorf("IsUnderutilized(%v, %v, %d) = %v, want %v",
					c.planConfig, c.usagePct, c.daysToEnd, got, c.want)
			}
		})
	}
}
