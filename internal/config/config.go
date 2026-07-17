// Package config carrega a configuração do AI Provider Pool.
//
// 12-factor: variáveis de ambiente como fonte primária. Defaults sensatos
// para desenvolvimento local. Beego conf/app.conf é respeitado como camada
// secundária onde aplicável.
//
// Profiles: a distinção dev/prod acontece via env vars (LOG_LEVEL, DB_DSN,
// ENABLE_METRICS). Não há arquivos de profile separados no MVP.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config agrega toda a configuração de runtime da aplicação.
//
// Campos sensíveis (senhas, KEK) NUNCA são logados. O campo KEKPath aponta
// para um arquivo montado readOnly via Secret/ESO (spec §3.3.2).
type Config struct {
	// HTTP
	HTTPAddr          string        // ex.: ":8080"
	ReadTimeout       time.Duration // 0 em produção p/ permitir streaming longo
	ReadHeaderTimeout time.Duration // sempre setado (proteção slowloris)
	WriteTimeout      time.Duration // 0 (streaming SSE dura minutos)
	IdleTimeout       time.Duration

	// Proxy
	UpstreamDialTimeout           time.Duration
	UpstreamTLSHandshakeTimeout   time.Duration
	UpstreamResponseHeaderTimeout time.Duration
	UpstreamOverallTimeout        time.Duration // overall cap por request (context)
	UpstreamFirstByteTimeout      time.Duration // tempo até primeiro chunk útil
	UpstreamIdleChunkTimeout      time.Duration // entre chunks (streaming)
	UpstreamMaxIdleConnsPerHost   int
	RequestBodyMaxBytes           int64 // 413 se exceder (defesa DoS)

	// Graceful shutdown
	ShutdownTimeout time.Duration
	PreStopSleep    time.Duration // tempo para o Ingress parar de rotear antes do SIGTERM efetivo

	// Database
	DBDSN             string // postgres://... ou URL completa
	DBMaxConns        int32
	DBMinConns        int32
	DBConnMaxLifetime time.Duration

	// Segredos (paths para arquivos readOnly montados via Secret/ESO)
	KEKPath         string // arquivo com a KEK (master) para envelope encryption
	TokenPepperPath string // arquivo com pepper para HMAC de tokens
	// TokenPepperInline é o pepper via env (dev). Se ambos (path e inline)
	// estiverem setados, o PATH tem precedência. Em prod, exigir path.
	TokenPepperInline string
	// SSRFAllowInsecureHTTP permite http:// em providers.base_url (DEV APENAS).
	// Em prod DEVE ser false. NÃO desativa o IP blocklist.
	SSRFAllowInsecureHTTP bool

	// PublicURL é a URL externa do serviço (ex.: https://ai-pool.exemplo.com).
	// Usada para gerar links de convite e configuração de clientes. Se vazia,
	// fazemos fallback para o Host do request (menos seguro — host header
	// injection). Em produção, configurar explicitamente.
	PublicURL string

	// Observabilidade
	LogLevel  string // debug | info | warn | error
	LogFormat string // json | text

	// Afonia/Rate-limit defaults
	AffinityTTL        time.Duration
	AffinityMaxPerUser int
	RateLimitBurst     int
	RateLimitPerMin    int

	// Organização default (multi-tenant ready)
	DefaultOrgID int64

	// Feature flags (preparação ROADMAP)
	EnableMetrics bool // endpoint /metrics (off no MVP, spec §3.5.4)
	DevMode       bool // degradações amigáveis em desenvolvimento
}

// Load carrega a configuração do ambiente. Retorna erro se campos OBRIGATÓRIOS
// estão faltando em modo produção (DevMode=false).
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:          getenv("HTTP_ADDR", ":8080"),
		ReadTimeout:       getduration("HTTP_READ_TIMEOUT", 60*time.Second),
		ReadHeaderTimeout: getduration("HTTP_READ_HEADER_TIMEOUT", 10*time.Second),
		WriteTimeout:      getduration("HTTP_WRITE_TIMEOUT", 0),
		IdleTimeout:       getduration("HTTP_IDLE_TIMEOUT", 120*time.Second),

		UpstreamDialTimeout:           getduration("UPSTREAM_DIAL_TIMEOUT", 5*time.Second),
		UpstreamTLSHandshakeTimeout:   getduration("UPSTREAM_TLS_HANDSHAKE_TIMEOUT", 5*time.Second),
		UpstreamResponseHeaderTimeout: getduration("UPSTREAM_RESPONSE_HEADER_TIMEOUT", 5*time.Minute),
		UpstreamOverallTimeout:        getduration("UPSTREAM_OVERALL_TIMEOUT", 10*time.Minute),
		UpstreamFirstByteTimeout:      getduration("UPSTREAM_FIRST_BYTE_TIMEOUT", 5*time.Minute),
		UpstreamIdleChunkTimeout:      getduration("UPSTREAM_IDLE_CHUNK_TIMEOUT", 3*time.Minute),
		UpstreamMaxIdleConnsPerHost:   getenvInt("UPSTREAM_MAX_IDLE_CONNS_PER_HOST", 100),
		RequestBodyMaxBytes:           getenvInt64("REQUEST_BODY_MAX_BYTES", 10*1024*1024),

		ShutdownTimeout: getduration("SHUTDOWN_TIMEOUT", 60*time.Second),
		PreStopSleep:    getduration("PRESTOP_SLEEP", 10*time.Second),

		DBDSN:             getenv("DATABASE_URL", getenv("DB_DSN", "")),
		DBMaxConns:        clampInt32(getenvInt("DB_MAX_CONNS", 60)),
		DBMinConns:        clampInt32(getenvInt("DB_MIN_CONNS", 5)),
		DBConnMaxLifetime: getduration("DB_CONN_MAX_LIFETIME", time.Hour),

		KEKPath:           getenv("KEK_PATH", ""),
		TokenPepperPath:   getenv("TOKEN_PEPPER_PATH", ""),
		TokenPepperInline: getenv("AIPP_TOKEN_PEPPER", ""),
		// Em DEV_MODE=true, permitimos http:// para provedores locais por default.
		// Override explícito via SSRF_ALLOW_HTTP=true|false.
		SSRFAllowInsecureHTTP: getbool("SSRF_ALLOW_HTTP", true),

		PublicURL: strings.TrimRight(getenv("AIPP_PUBLIC_URL", ""), "/"),

		LogLevel:  strings.ToLower(getenv("LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(getenv("LOG_FORMAT", "json")),

		AffinityTTL:        getduration("AFFINITY_TTL", 30*time.Minute),
		AffinityMaxPerUser: getenvInt("AFFINITY_MAX_PER_USER", 50),
		RateLimitBurst:     getenvInt("RATE_LIMIT_BURST", 10),
		RateLimitPerMin:    getenvInt("RATE_LIMIT_PER_MIN", 60),

		DefaultOrgID: getenvInt64("DEFAULT_ORG_ID", 1),

		EnableMetrics: getbool("ENABLE_METRICS", false),
		DevMode:       getbool("DEV_MODE", true),
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checa campos obrigatórios dependendo do modo.
func (c *Config) Validate() error {
	if !c.DevMode {
		var missing []string
		if c.DBDSN == "" {
			missing = append(missing, "DATABASE_URL|DB_DSN")
		}
		if c.KEKPath == "" {
			missing = append(missing, "KEK_PATH")
		}
		// Token pepper: em prod, preferimos path (arquivo readOnly via ESO).
		// Inline via env é aceitável mas desencorajado (vaza em ps/process listing).
		if c.TokenPepperPath == "" && c.TokenPepperInline == "" {
			missing = append(missing, "TOKEN_PEPPER_PATH (or AIPP_TOKEN_PEPPER)")
		}
		// Em prod, http:// para providers é PROIBIDO (mesmo se SSRF_ALLOW_HTTP=true).
		if c.SSRFAllowInsecureHTTP {
			return errors.New("config: SSRF_ALLOW_HTTP must be false in production")
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: missing required env vars in prod mode: %s", strings.Join(missing, ", "))
		}
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return errors.New("config: LOG_LEVEL must be one of debug|info|warn|error")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return errors.New("config: LOG_FORMAT must be json|text")
	}
	return nil
}

// LoadTokenPepper retorna o pepper efetivo (path tem precedência sobre inline).
//
// Em DEV_MODE sem nada configurado, retorna um pepper DEFAULT inseguro
// APENAS para possibilitar subir localmente sem config. Em produção o
// Validate() já rejeita config sem pepper.
//
// O pepper retornado DEVE ter pelo menos 32 bytes. O caller deve validar.
func (c *Config) LoadTokenPepper() ([]byte, error) {
	if c.TokenPepperPath != "" {
		b, err := os.ReadFile(c.TokenPepperPath)
		if err != nil {
			return nil, fmt.Errorf("config: read TOKEN_PEPPER_PATH: %w", err)
		}
		return b, nil
	}
	if c.TokenPepperInline != "" {
		return []byte(c.TokenPepperInline), nil
	}
	// Dev fallback — pepper fraco mas determinístico para dev.
	// PRODUÇÃO: o Validate() acima já impede chegar aqui.
	return []byte("DEV_MODE_INSECURE_PEPPER_DO_NOT_USE_IN_PROD_32B!"), nil
}

// === Helpers ===

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// clampInt32 converte int para int32 com bounds check, evitando overflow
// silencioso quando o valor vem de env var (gosec G115). Em 64-bit, int é
// int64; int32 tem range ±2.1B. Para config de pool DB isso é mais que
// suficiente, mas clamp previne comportamento indefinido se alguém setar
// DB_MAX_CONNS=9999999999999.
func clampInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}

func getenvInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getduration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getbool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
