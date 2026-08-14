package domain

import "time"

// ID é o identificador interno numérico de todas as entidades (BIGINT no PostgreSQL).
// Preferimos int64 alinhado ao "BIGINT GENERATED ALWAYS AS IDENTITY" do schema.
type ID int64

// OrganizationID identifica o tenant. No MVP há apenas a organização default (id=1),
// mas todas as entidades carregam este campo para pavimentar multi-tenancy futuro
// sem migração cara (spec §3.1.4).
type OrganizationID ID

// DefaultOrganizationID é a única organização do MVP.
const DefaultOrganizationID OrganizationID = 1

// User representa um usuário do pool (admin ou usuário comum).
//
// Senhas são hasheadas com Argon2id (spec §3.3.5). Tokens de API (sk-aipp-...)
// são entidades separadas (APIToken) e NÃO ficam armazenados nesta struct.
type User struct {
	ID           ID
	OrgID        OrganizationID
	Username     string
	PasswordHash []byte // Argon2id (nunca plaintext)
	Role         Role
	Status       UserStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DisabledAt   *time.Time // não-null quando Status == UserStatusDisabled
}

// Role é o papel do usuário. MVP tem apenas admin e user (spec §3.3.4).
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// UserStatus reflete o ciclo de vida do usuário.
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

// CanAccessPool retorna false se o usuário está desabilitado. O middleware de
// auth valida isto a cada request (DB hit), garantindo revogação imediata
// (spec §3.3.3).
func (u *User) CanAccessPool() bool {
	return u.Status == UserStatusActive && u.DisabledAt == nil
}

// APIToken é um token sk-aipp-... gerado pelo usuário para consumir o proxy.
//
// Estrutura (spec §3.3.3):
//
//	sk-aipp-[key_id 8 chars]-[secret 48 chars]
//
// O valor completo só é conhecido no momento da geração. Persistimos apenas:
//   - KeyID (público, lookup O(1) por índice UNIQUE)
//   - TokenHMAC (HMAC-SHA256 do secret com pepper externo — defesa em profundidade)
type APIToken struct {
	ID         ID
	UserID     ID
	OrgID      OrganizationID
	KeyID      string // parte pública, ex.: "AB12CD34"
	TokenHMAC  []byte // HMAC-SHA256(secret, pepper)
	Name       string // rótulo humano ("meu-token-dev")
	Status     APITokenStatus
	CreatedAt  time.Time
	ExpiresAt  *time.Time // nil = sem expiração (desencorajado)
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type APITokenStatus string

const (
	APITokenStatusActive  APITokenStatus = "active"
	APITokenStatusRevoked APITokenStatus = "revoked"
	APITokenStatusExpired APITokenStatus = "expired"
)

// IsValid checa status + expiração. Usado no middleware de auth (DB hit por request).
func (t *APIToken) IsValid(now time.Time) bool {
	if t.Status != APITokenStatusActive {
		return false
	}
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
		return false
	}
	return true
}

// Provider é um provedor de LLM (Gemini, Mistral, DeepSeek, Z.ai, Kimi).
// Todos são consumidos via schema OpenAI-compatible (spec §3.2.1).
type Provider struct {
	ID              ID
	OrgID           OrganizationID
	Name            string // ex.: "DeepSeek"
	Slug            string // ex.: "deepseek" (UNIQUE por org)
	BaseURL         string // com SSRF guard aplicado (spec §3.3.6)
	APISpec         string // "openai" | "gemini-native" ... pavimenta parsers
	UserAgentGlobal *string
	IsActive        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ProviderKey é uma chave de API de provedor (o ativo mais sensível do sistema).
//
// O valor criptografado fica em EncryptedValue (AES-256-GCM envelope encryption,
// spec §3.3.2). O KEK nunca vive no DB.
type ProviderKey struct {
	ID              ID
	ProviderID      ID
	OrgID           OrganizationID
	Label           string // rótulo humano
	EncryptedValue  []byte // AES-256-GCM ciphertext
	EncryptionKeyID string // ex.: "v1" — suporta rotação de KEK
	Status          KeyStatus
	DisabledReason  *KeyDisabledReason
	DisabledAt      *time.Time
	DisabledUntil   *time.Time // não-null quando Status == KeyStatusAutoDisabled (reativação agendada)
	ErrorCount      int
	// ConsecutiveErrors conta falhas consecutivas (reset em qualquer sucesso).
	// Distinto de ErrorCount (janela deslizante): usado para critério rápido
	// "N falhas seguidas" na decisão de auto-disable (spec §3.2.4).
	ConsecutiveErrors int
	LastErrorAt       *time.Time
	LastErrorHint     *string // data extraída do parse (liberação de cota)
	LastUsedAt        *time.Time
	UserAgentOverride *string
	// Campos de plano (issue #19, RF-01): chaves de assinatura (Z.ai Coding
	// Plan, Kimi Code) têm ciclo com cotas em janelas ancoradas na renovação.
	// Nullable — chaves pay-as-you-go não têm ciclo de plano.
	PlanName        *string    // ex.: "GLM Coding Lite"
	PlanRenewalDate *time.Time // DATE no DB (âncora do ciclo semanal/mensal)
	PlanQuotaTokens *int64     // denominador configurado pelo admin (tokens/ciclo)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// KeyStatus reflete a máquina de estados da chave (spec §3.2.4).
type KeyStatus string

const (
	// KeyStatusEnabled está no pool ativo — candidata a LRU e failover.
	KeyStatusEnabled KeyStatus = "enabled"
	// KeyStatusAutoDisabled foi desabilitada pelo sistema após threshold de erros.
	// Apenas este estado é reativável pelo scheduler (disabled_until).
	KeyStatusAutoDisabled KeyStatus = "auto_disabled"
	// KeyStatusManualDisabled foi desabilitada pelo admin. Ignora disabled_until.
	KeyStatusManualDisabled KeyStatus = "manual_disabled"
)

// KeyDisabledReason documenta o porquê do disable — útil para dashboard e auditoria.
type KeyDisabledReason string

const (
	ReasonManual           KeyDisabledReason = "manual"
	ReasonQuotaExceeded    KeyDisabledReason = "quota_exceeded"
	ReasonRateLimited      KeyDisabledReason = "rate_limited"
	ReasonErrorThreshold   KeyDisabledReason = "error_threshold"
	ReasonAuthFailed       KeyDisabledReason = "auth_failed"
	ReasonParseFailedRetry KeyDisabledReason = "parse_failed_retry"
)

// IsUsable indica se a chave está elegível para seleção (LRU/afinidade/failover).
// Considera reativação lazy: se disabled_until já passou, trata como reativável.
func (k *ProviderKey) IsUsable(now time.Time) bool {
	switch k.Status {
	case KeyStatusEnabled:
		return true
	case KeyStatusAutoDisabled:
		// Lazy reativação (spec §3.2.4): se a data passou, é usável (o scheduler
		// ou o próximo request irão efetivar a transição para Enabled).
		return k.DisabledUntil != nil && now.After(*k.DisabledUntil)
	case KeyStatusManualDisabled:
		return false
	}
	return false
}

// Model é um modelo de LLM oferecido por um provedor.
// Models.context_window pavimenta failover cross-provider futuro (spec §4).
type Model struct {
	ID            ID
	ProviderID    ID
	OrgID         OrganizationID
	Name          string // nome real no provedor, ex.: "deepseek-chat"
	DisplayName   *string
	ContextWindow *int // tokens (janela total: input+output suportado)
	// OutputLimit é o máximo de tokens que o modelo pode GERAR por resposta
	// (ex.: 8192). Distinto de ContextWindow: GLM-5.2 tem 128k de contexto mas
	// ~8k de output. Usado para montar o bloco "limit.output" da config do
	// OpenCode e para calibrar max_tokens nos requests. Nullable.
	OutputLimit *int     // tokens de saída por resposta
	Modalities  []string // ex.: ["text","image"] — vazio = texto apenas
	IsActive    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IsMultimodal retorna true se o modelo aceita imagem como entrada.
func (m *Model) IsMultimodal() bool {
	for _, mod := range m.Modalities {
		if mod == "image" {
			return true
		}
	}
	return false
}
