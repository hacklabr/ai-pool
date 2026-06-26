package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/rafael/ai-pool/internal/config"
)

// =============================================================================
// HTTP Client para repasse ao upstream (provedores de LLM).
//
// Princípios (spec §3.2.1, Backend Architect Turno 1 §3.3):
//   - ÚNICO http.Client global, compartilhado por toda a app (singleton).
//   - Connection pooling no Transport (MaxIdleConnsPerHost, keepalive).
//   - SEM Client.Timeout global — controle via context (streaming longo).
//   - ForceAttemptHTTP2: true (provedores suportam; reduz latência).
//   - ResponseHeaderTimeout: tempo até o provedor enviar status + headers
//     (este é o "first-byte" para status — NÃO conta o body do streaming).
//
// Otimizações:
//   - DialContext com timeout curto (5-10s): detecta provedor offline rápido.
//   - TLSHandshakeTimeout: idem.
//   - IdleConnTimeout: 90s (fecha conexões idle para liberar FDs).
//   - MaxIdleConns global: limite rasoável para 5 provedores × 100 conns/host.
// =============================================================================

// NewUpstreamClient cria o http.Client para repasse aos provedores.
//
// Em F7: hookar aqui um http.RoundTripper decorado com:
//   - Métricas Prometheus (latência, erros por provider).
//   - Logging estruturado de requests (sem body, sem Authorization).
//   - Sanitização de headers no DumpRequest (debug mode).
func NewUpstreamClient(cfg *config.Config) *http.Client {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.UpstreamDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		TLSHandshakeTimeout:   cfg.UpstreamTLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.UpstreamResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,

		// Connection pooling — crítico para evitar TCP/TLS handshake por request.
		MaxIdleConns:        cfg.UpstreamMaxIdleConnsPerHost * 5, // ~5 providers
		MaxIdleConnsPerHost: cfg.UpstreamMaxIdleConnsPerHost,
		MaxConnsPerHost:     0, // 0 = ilimitado (cada request cria conn se pool esgotar)

		ForceAttemptHTTP2: true,
		// Em produção, TLS verify é OBRIGATÓRIO. Em DEV_MODE, ainda mantemos
		// verify=true (provedores reais sempre têm cert válido; testes com mock
		// podem usar httptest.NewTLSServer que emite cert válido).
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// InsecureSkipVerify fica em false sempre. Provedor com cert inválido
			// deve falhar — melhor do que permitir MITM silencioso.
		},
	}

	return &http.Client{
		Transport: t,
		// Timeout ZERO = sem timeout no client. O controle de duração fica via
		// context (overall, first-byte, idle) passado a cada Do().
		// Isto é CRÍTICO para streaming SSE: um Client.Timeout cortaria
		// conexões longas (chats de minutos).
		Timeout: 0,
	}
}
