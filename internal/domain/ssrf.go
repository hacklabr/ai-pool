package domain

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// =============================================================================
// SSRF Guard — validação de URLs de provedores (spec §3.3.6, Security §8.2).
//
// O proxy repassa requisições para URLs configuradas por admin (em providers.base_url).
// Mesmo admin confiável, defesa em profundidade: se o DB for comprometido via
// SQLi, um atacante pode tentar configurar uma URL apontando para:
//   - 169.254.169.254 (metadata cloud — AWS/GCP/Azure)
//   - 127.0.0.1 / localhost (serviços internos)
//   - 10.x / 172.16-31.x / 192.168.x (RFC1918 — rede interna)
//   - ::1, fc00::/7, fe80::/10 (IPv6 equivalents)
//
// ValidateProviderURL é a versão BÁSICA para F3:
//   - Rejeita scheme != http(s)
//   - Rejeita hosts que resolvem para IPs bloqueados (loopback, privado,
//     link-local, multicast, unspecified, metadata cloud 169.254.x).
//   - Requer porta default se ausente (80/443).
//
// Hardening completo (F7, T22):
//   - Re-resolução por request (defesa DNS rebinding)
//   - Allowlist explícita de hostnames por org
//   - Pin de IP resolvido na conexão (evita rebinding entre resolve e dial)
// =============================================================================

// SSRFOption configura ValidateProviderURL.
type SSRFOption func(*ssrfConfig)

type ssrfConfig struct {
	allowInsecureHTTP bool
}

// WithSSRFAllowInsecureHTTP permite scheme http (para dev com provedor local).
// Em produção, DEVE ser false. Esse flag NÃO desativa o IP blocklist.
func WithSSRFAllowInsecureHTTP(b bool) SSRFOption {
	return func(c *ssrfConfig) { c.allowInsecureHTTP = b }
}

// ErrSSRFSchemeBlocked scheme não permitido.
var ErrSSRFSchemeBlocked = errors.New("ssrf: scheme blocked")

// ErrSSRFHostBlocked host/IP bloqueado (privado, loopback, metadata, etc.).
var ErrSSRFHostBlocked = errors.New("ssrf: host blocked")

// ErrSSRFHostUnresolvedable hostname não resolve.
var ErrSSRFHostUnresolvedable = errors.New("ssrf: host unresolved")

// ValidateProviderURL valida a URL de um provedor contra regras de SSRF.
//
// Resolve o hostname via net.LookupIP (que faz a consulta DNS). Isto é uma
// validação estática — a conexão real pode re-resolver o hostname para outro IP
// (DNS rebinding). Para F7, a defesa completa é resolver e conectar ao IP
// diretamente. Para F3, esta validação cobre o vetor mais comum.
//
// Parâmetros:
//   - rawURL: string a validar.
//   - opts:   configurações (ex.: WithSSRFAllowInsecureHTTP para dev).
func ValidateProviderURL(rawURL string, opts ...SSRFOption) error {
	cfg := &ssrfConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	if rawURL == "" {
		return fmt.Errorf("ssrf: empty url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ssrf: invalid url: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		// sempre permitido
	case "http":
		if !cfg.allowInsecureHTTP {
			return fmt.Errorf("%w: http not allowed (use https or enable dev mode)", ErrSSRFSchemeBlocked)
		}
	default:
		return fmt.Errorf("%w: %q", ErrSSRFSchemeBlocked, scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: missing host")
	}

	// Caso 1: host já é IP literal.
	if ip := net.ParseIP(host); ip != nil {
		if err := checkBlockedIP(ip); err != nil {
			return err
		}
		return nil
	}

	// Caso 2: hostname — resolve e valida TODOS os IPs retornados.
	// Se QUALQUER resolução aponta para IP bloqueado, rejeitamos (defesa contra
	// registros DNS que misturam IPs válidos e internos).
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSSRFHostUnresolvedable, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %s resolved to zero IPs", ErrSSRFHostUnresolvedable, host)
	}
	for _, ip := range ips {
		if err := checkBlockedIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// checkBlockedIP retorna erro se o IP pertence a uma faixa bloqueada.
//
// Faixas bloqueadas:
//   - Loopback: 127.0.0.0/8, ::1
//   - Privado (RFC1918): 10/8, 172.16/12, 192.168/16; IPv6 fc00::/7 (ULA)
//   - Link-local: 169.254/16 (INCLUI 169.254.169.254 metadata cloud), fe80::/10
//   - Multicast: 224/4, ff00::/8
//   - Unspecified: 0.0.0.0, ::
//   - IPv6 IPv4-mapped (::ffff:x.x.x.x) — extrai o IPv4 e valida
func checkBlockedIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("%w: nil ip", ErrSSRFHostBlocked)
	}

	// IPv4-mapped IPv6 → extrair IPv4 e validar.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	switch {
	case ip.IsLoopback():
		return fmt.Errorf("%w: loopback %s", ErrSSRFHostBlocked, ip)
	case ip.IsPrivate():
		return fmt.Errorf("%w: private %s", ErrSSRFHostBlocked, ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.x (incluindo metadata cloud) cai aqui.
		return fmt.Errorf("%w: link-local %s", ErrSSRFHostBlocked, ip)
	case ip.IsMulticast():
		return fmt.Errorf("%w: multicast %s", ErrSSRFHostBlocked, ip)
	case ip.IsUnspecified():
		return fmt.Errorf("%w: unspecified %s", ErrSSRFHostBlocked, ip)
	}
	return nil
}

// IsSSRFBlocked retorna true se err é um erro de bloqueio do SSRF guard
// (host ou scheme). Útil para o caller distinguir "configuração inválida"
// (rejeitar) de "falha temporária de DNS" (tentar novamente).
func IsSSRFBlocked(err error) bool {
	return errors.Is(err, ErrSSRFHostBlocked) || errors.Is(err, ErrSSRFSchemeBlocked)
}
