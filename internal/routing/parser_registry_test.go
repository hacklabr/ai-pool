package routing

import (
	"testing"
	"time"
)

func TestDefaultErrorParser_ISO8601(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`rate limit exceeded; please retry after 2026-12-31T23:59:59Z`)
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok=true for ISO 8601")
	}
	want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDefaultErrorParser_ISO8601WithOffset(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`quota resets at 2026-12-31T23:59:59-03:00`)
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok")
	}
	// 2026-12-31T23:59:59-03:00 = 2027-01-01T02:59:59Z
	want := time.Date(2027, 1, 1, 2, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v UTC, want %v UTC", got.UTC(), want.UTC())
	}
}

func TestDefaultErrorParser_RelativeSeconds(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`too many requests; retry in 60 seconds`)
	before := time.Now().UTC()
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok for 'in N seconds'")
	}
	if d := got.Sub(before); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("delta = %v, want ~60s", d)
	}
}

func TestDefaultErrorParser_RelativeMinutes(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`rate limited; try again in 5 minutes`)
	before := time.Now().UTC()
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok")
	}
	if d := got.Sub(before); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("delta = %v, want ~5min", d)
	}
}

func TestDefaultErrorParser_RetryAfterHeader(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`{"error":{"retry-after": 30}}`)
	before := time.Now().UTC()
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok for retry-after")
	}
	if d := got.Sub(before); d < 25*time.Second || d > 35*time.Second {
		t.Errorf("delta = %v, want ~30s", d)
	}
}

func TestDefaultErrorParser_UnixEpoch(t *testing.T) {
	p := &DefaultErrorParser{}
	// Epoch ~1h no futuro.
	future := time.Now().UTC().Add(time.Hour).Unix()
	body := []byte(`limit resets at ` + itoaEpoch(future))
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok for epoch")
	}
	if d := got.Sub(time.Now().UTC()); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("delta = %v, want ~1h", d)
	}
}

func TestDefaultErrorParser_DateOnly(t *testing.T) {
	p := &DefaultErrorParser{}
	// Data futura simples.
	future := time.Now().UTC().AddDate(0, 0, 7)
	body := []byte(`quota resets on ` + future.Format("2006-01-02"))
	got, ok := p.ParseResetDate(429, body)
	if !ok {
		t.Fatal("expected ok for date only")
	}
	want := time.Date(future.Year(), future.Month(), future.Day(), 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDefaultErrorParser_NoMatch(t *testing.T) {
	p := &DefaultErrorParser{}
	body := []byte(`something went wrong; please try again`)
	_, ok := p.ParseResetDate(500, body)
	if ok {
		t.Error("expected ok=false for body without date")
	}
}

func TestParserRegistry_LookupAndFallback(t *testing.T) {
	r := NewParserRegistry()
	body := []byte(`retry in 30 seconds`)

	// Sem registrar nada → usa default.
	_, ok := r.ParseResetDate("nonexistent", 429, body)
	if !ok {
		t.Error("default should match 'in N seconds'")
	}

	// Registra stub para "customprovider" que sempre retorna false.
	custom := &neverMatchParser{}
	r.Register("customprovider", custom)
	_, ok = r.ParseResetDate("customprovider", 429, body)
	if ok {
		t.Error("custom parser should not match")
	}
}

func TestParserRegistry_CaseInsensitiveSlug(t *testing.T) {
	r := NewParserRegistry()
	custom := &neverMatchParser{}
	r.Register("DeepSeek", custom)
	// Lookup com slug em case diferente deve achar.
	_, ok := r.ParseResetDate("deepseek", 429, []byte(`anything`))
	if ok {
		t.Error("custom parser should not match")
	}
}

func TestRegisterKnownProviders_RegistersAll(t *testing.T) {
	r := NewParserRegistry()
	RegisterKnownProviders(r)
	body := []byte(`retry in 30 seconds`)
	for _, slug := range []string{"gemini", "mistral", "deepseek", "zai", "kimi"} {
		if _, ok := r.ParseResetDate(slug, 429, body); !ok {
			t.Errorf("known provider %q should match default parser", slug)
		}
	}
}

// neverMatchParser implementa ErrorParser mas sempre retorna false.
type neverMatchParser struct{}

func (neverMatchParser) ParseResetDate(_ int, _ []byte) (time.Time, bool) {
	return time.Time{}, false
}

// itoaEpoch é um helper para formatar epoch sem import strconv (já importado em
// outros testes do pacote; mantemos aqui para isolamento visual).
func itoaEpoch(n int64) string {
	// strconv.FormatInt em bases.go — simplificado inline.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
