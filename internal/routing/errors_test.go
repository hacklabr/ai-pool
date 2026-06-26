package routing

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestClassifyStatusCode_Success(t *testing.T) {
	for code := 200; code < 300; code++ {
		if got := ClassifyStatusCode(code); got != FailureNone {
			t.Errorf("status %d: got %v, want FailureNone", code, got)
		}
	}
}

func TestClassifyStatusCode_ClientErrors(t *testing.T) {
	// 400/404/413/422/415 → NÃO failover (problema do cliente).
	for _, code := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity,
		http.StatusUnsupportedMediaType,
	} {
		if got := ClassifyStatusCode(code); got != FailureClient {
			t.Errorf("status %d: got %v, want FailureClient", code, got)
		}
	}
}

func TestClassifyStatusCode_RetryableErrors(t *testing.T) {
	// 401/403/429/408/5xx → DISPARAM failover.
	for _, code := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusRequestTimeout,
		http.StatusConflict,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if got := ClassifyStatusCode(code); got != FailureRetryable {
			t.Errorf("status %d: got %v, want FailureRetryable", code, got)
		}
	}
}

func TestClassifyError_NilIsNone(t *testing.T) {
	if got := ClassifyError(nil); got != FailureNone {
		t.Errorf("nil err: got %v, want FailureNone", got)
	}
}

func TestClassifyError_NetworkIsRetryable(t *testing.T) {
	if got := ClassifyError(errors.New("connection refused")); got != FailureRetryable {
		t.Errorf("network err: got %v, want FailureRetryable", got)
	}
}

func TestIsRetryable(t *testing.T) {
	if !IsRetryable(http.StatusTooManyRequests, nil) {
		t.Error("429 should be retryable")
	}
	if IsRetryable(http.StatusBadRequest, nil) {
		t.Error("400 should NOT be retryable")
	}
	if !IsRetryable(0, errors.New("timeout")) {
		t.Error("network error should be retryable")
	}
}

func TestBackoff_DelayShape(t *testing.T) {
	bc := DefaultBackoffConfig()
	// Delay(attempt) deve estar em [0, cap].
	for attempt := 0; attempt < 10; attempt++ {
		d := bc.Delay(attempt)
		if d < 0 || d > bc.Cap {
			t.Errorf("Delay(%d) = %v, want [0, %v]", attempt, d, bc.Cap)
		}
	}
}

func TestBackoff_MonotonicGrowthAverage(t *testing.T) {
	// Verifica que a MÉDIA cresce com attempt. Jitter torna cada amostra
	// aleatória, mas a MÉDIA (n amostras) deve crescer monotonicamente até
	// atingir o cap.
	bc := DefaultBackoffConfig()
	const samples = 1000
	avg := func(attempt int) time.Duration {
		var sum time.Duration
		for i := 0; i < samples; i++ {
			sum += bc.Delay(attempt)
		}
		return sum / samples
	}
	a0 := avg(0)
	a1 := avg(1)
	a2 := avg(2)
	if !(a0 <= a1 && a1 <= a2) {
		t.Errorf("avg should grow: a0=%v a1=%v a2=%v", a0, a1, a2)
	}
	// Em attempts altos, deve bater o cap.
	aHi := avg(50)
	if aHi > bc.Cap {
		t.Errorf("avg at attempt 50 = %v, must not exceed cap %v", aHi, bc.Cap)
	}
}

// TestRandInt63n determinístico para sanity.
func TestRandInt63n(t *testing.T) {
	for _, n := range []int64{1, 10, 1000, 1_000_000} {
		v, err := randInt63n(n)
		if err != nil {
			t.Fatalf("randInt63n(%d): %v", n, err)
		}
		if v < 0 || v >= n {
			t.Errorf("randInt63n(%d) = %d, want [0, %d)", n, v, n)
		}
	}
	if _, err := randInt63n(0); err == nil {
		t.Error("randInt63n(0) should error")
	}
}
