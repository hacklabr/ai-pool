package routing

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// =============================================================================
// Classificação da camada de limite (issue #19, RF-03).
//
// Os provedores usados pelo pool (Z.ai GLM Coding Plan e Kimi Code/Membership)
// são assinaturas com cotas em janelas (5h / semanal / mensal) ancoradas na
// data da assinatura, e NENHUM expõe API de cota/consumo. A camada do limite
// é inferida da MENSAGEM DE ERRO upstream (códigos do Z.ai, textos do Kimi).
//
// Função PURA (sem I/O, sem estado) — testável com as amostras reais dos
// provedores (ver limit_layer_test.go).
// =============================================================================

// Camadas de limite persistidas em key_quota_measurements.limit_layer e no
// metadata do audit_log.
const (
	LimitLayerWindow5h = "window_5h"
	LimitLayerWeekly   = "weekly"
	LimitLayerMonthly  = "monthly"
	LimitLayerBalance  = "balance"
	LimitLayerTier     = "tier"
	LimitLayerOther    = "other"
)

// regexes compiladas uma vez (imutáveis; thread-safe após MustCompile).
var (
	// Z.ai: código numérico no corpo JSON — "code":"1308"
	reZaiCode = regexp.MustCompile(`"code"\s*:\s*"?(\d{3,4})"?`)
	// Z.ai: next_flush_time (RFC3339) — quando presente, é o reset da janela.
	reNextFlushTime = regexp.MustCompile(`next_flush_time"?\s*:\s*"?(\d{4}-\d{2}-\d{2}T[^"]+?)"`)
	// RFC3339 genérico na mensagem — "limit will be reset at 2026-08-13T19:00:00Z"
	reResetAtRFC3339 = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2}))`)
	// Texto de unidade de janela — "in 5 hours", "7 days", "weekly", "monthly"
	reUnitHours = regexp.MustCompile(`(?i)\b\d+\s+hours?\b|\bhourly\b`)
	reUnitWeek  = regexp.MustCompile(`(?i)\b\d+\s+days?\b|\bweekly\b|\bweek\b`)
	reUnitMonth = regexp.MustCompile(`(?i)\bmonthly\b|\bmonth\b`)
)

// ClassifyLimitLayer classifica a camada do limite (5h/semanal/mensal/saldo/
// tier/other) a partir do erro upstream. Retorna também resetAt (data de
// liberação extraída do body, quando presente).
//
// Regras por provedor:
//
//	Z.ai (slug "zai"/"z-ai") — por código no body:
//	  1113          → balance (saldo insuficiente)
//	  1309          → other (plano expirado)
//	  1308, 1310    → janela longa (weekly/monthly) pelo texto de unidade;
//	                  fallback weekly
//	  1316-1318     → window_5h (códigos da janela de 5h)
//	  1319-1321     → pelo texto de unidade; sem unidade → other
//	  resetAt       → next_flush_time ou RFC3339 na mensagem
//
//	Kimi (slug "kimi"/"moonshot") — por texto:
//	  "monthly usage limit"                    → monthly (checado ANTES de
//	  "billing cycle" — o texto mensal também contém "billing cycle")
//	  "billing cycle"                          → weekly
//	  "usage limit for this period"            → window_5h
//	  "does not have access" / 401 tier errors → tier
//	  resto                                    → other
//
// Provedor desconhecido → other (resetAt ainda é extraído best-effort).
func ClassifyLimitLayer(providerSlug string, statusCode int, errBody string) (layer string, resetAt *time.Time) {
	resetAt = extractResetAt(errBody)
	switch strings.ToLower(strings.TrimSpace(providerSlug)) {
	case "zai", "z-ai":
		return classifyZai(errBody), resetAt
	case "kimi", "moonshot":
		return classifyKimi(statusCode, errBody), resetAt
	default:
		return LimitLayerOther, resetAt
	}
}

// classifyZai classifica pelo código numérico do erro (Z.ai GLM Coding Plan).
func classifyZai(body string) string {
	m := reZaiCode.FindStringSubmatch(body)
	if len(m) < 2 {
		return LimitLayerOther
	}
	code := m[1]
	switch code {
	case "1113":
		return LimitLayerBalance
	case "1309":
		// Plano expirado — não é uma janela de cota.
		return LimitLayerOther
	case "1308", "1310":
		// Janela longa; o texto de unidade decide (weekly/monthly). O corpo
		// real do 1308 pode citar "in 5 hours" (janela curta) — respeitamos
		// o texto quando presente.
		if l, ok := unitLayer(body); ok {
			return l
		}
		return LimitLayerWeekly
	case "1316", "1317", "1318":
		if l, ok := unitLayer(body); ok {
			return l
		}
		return LimitLayerWindow5h
	case "1319", "1320", "1321":
		if l, ok := unitLayer(body); ok {
			return l
		}
		return LimitLayerOther
	default:
		return LimitLayerOther
	}
}

// classifyKimi classifica pelo texto da mensagem (Kimi Code/Membership).
// ORDEM IMPORTA: o texto mensal ("monthly usage limit for this billing
// cycle") também contém "billing cycle" — monthly é checado primeiro.
func classifyKimi(statusCode int, body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "monthly usage limit"):
		return LimitLayerMonthly
	case strings.Contains(lower, "billing cycle"):
		return LimitLayerWeekly
	case strings.Contains(lower, "usage limit for this period"):
		return LimitLayerWindow5h
	case strings.Contains(lower, "does not have access"):
		return LimitLayerTier
	case statusCode == 401:
		// 401 sem texto conhecido — erro de tier/assinatura.
		return LimitLayerTier
	default:
		return LimitLayerOther
	}
}

// unitLayer mapeia o texto de unidade da janela ("in 5 hours", "7 days",
// "weekly", "monthly") para a camada. Retorna ok=false se nenhum texto casa.
func unitLayer(body string) (string, bool) {
	switch {
	case reUnitMonth.MatchString(body):
		return LimitLayerMonthly, true
	case reUnitWeek.MatchString(body):
		return LimitLayerWeekly, true
	case reUnitHours.MatchString(body):
		return LimitLayerWindow5h, true
	}
	return "", false
}

// extractResetAt extrai a data de liberação da cota do body: prefere o campo
// next_flush_time (Z.ai); fallback para qualquer RFC3339 na mensagem.
// Retorna nil se não houver data parseável.
func extractResetAt(body string) *time.Time {
	if s := reNextFlushTime.FindStringSubmatch(body); len(s) >= 2 {
		if t, err := parseRFC3339Loose(s[1]); err == nil {
			return &t
		}
	}
	if s := reResetAtRFC3339.FindString(body); s != "" {
		if t, err := parseRFC3339Loose(s); err == nil {
			return &t
		}
	}
	return nil
}

// parseRFC3339Loose parseia RFC3339 (com ou sem fração, com ou sem dois
// pontos no offset).
func parseRFC3339Loose(s string) (time.Time, error) {
	s = strings.TrimSuffix(s, `"`)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parseRFC3339Loose: formato não reconhecido: %q", s)
}

// ClassifyErrorClass mapeia o status/erro upstream para a classe usada em
// usage_logs.error_class (issue #19, de-quebra b):
//
//	429          → 'quota' quando a ClassifyLimitLayer identifica uma janela
//	               de plano (window_5h/weekly/monthly/balance); senão 'rate_limit'
//	401/403      → 'auth'
//	>= 500       → '5xx'
//	outros       → nil (sucesso ou erro do cliente — sem classe)
//
// errBody é a mensagem upstream sanitizada ou cru (quando disponível); pode
// ser vazia — sem corpo, 429 fica 'rate_limit'.
func ClassifyErrorClass(statusCode int, providerSlug string, errBody string) *string {
	var class string
	switch {
	case statusCode == 429:
		layer, _ := ClassifyLimitLayer(providerSlug, statusCode, errBody)
		switch layer {
		case LimitLayerWindow5h, LimitLayerWeekly, LimitLayerMonthly, LimitLayerBalance:
			class = "quota"
		default:
			class = "rate_limit"
		}
	case statusCode == 401 || statusCode == 403:
		class = "auth"
	case statusCode >= 500:
		class = "5xx"
	default:
		return nil
	}
	return &class
}
