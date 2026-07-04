package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// =============================================================================
// Tokens de convite de usuários.
//
// Um convite é um segredo de uso único que permite a um novo usuário definir
// sua senha e, opcionalmente, configurar seu OpenCode automaticamente. O token
// raw é entregue ao admin no momento da criação (e exibido uma única vez); o
// DB guarda apenas seu hash SHA-256, seguindo o mesmo princípio dos tokens
// sk-aipp-... (spec §3.3.3).
//
// Formato do token raw:
//
//	inv-[prefixo 8 chars]-[segredo 32 bytes base64 URL-safe]
//
// Exemplo: inv-AbCdEf12-aB8dEfGhIjKlMnOpQrStUvWxYz1234567890ab
//
// Características:
//   - Prefixo visível (8 chars) para listagens e auditoria.
//   - Segredo com 32 bytes (256 bits) de entropia — inviável de adivinhar.
//   - Hash SHA-256 persistente; comparação em tempo constante.
//   - Uso único: status muda para "used" após o aceite.
//   - Expiração fixa de 24h por padrão.
// =============================================================================

// InvitationTokenStatus reflete o ciclo de vida de um convite.
type InvitationTokenStatus string

const (
	InvitationStatusPending InvitationTokenStatus = "pending"
	InvitationStatusUsed    InvitationTokenStatus = "used"
	InvitationStatusRevoked InvitationTokenStatus = "revoked"
)

// InvitationToken representa um convite de usuário.
type InvitationToken struct {
	ID          ID
	OrgID       OrganizationID
	TokenHash   string // SHA-256 do token raw (hex)
	TokenPrefix string // 8 chars visíveis
	Username    string
	Role        Role
	Status      InvitationTokenStatus
	CreatedBy   ID
	UsedBy      ID
	UsedAt      *time.Time
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// InvitationTokenRaw contém o token recém-gerado e seus metadados.
// O valor raw só existe neste momento.
type InvitationTokenRaw struct {
	Raw    string
	Hash   string
	Prefix string
}

const (
	// InvitationPrefix é o prefixo legível do token de convite.
	InvitationPrefix = "inv-"

	// InvitationPrefixLength é o tamanho da parte pública visível.
	InvitationPrefixLength = 8

	// InvitationSecretBytes é a entropia do segredo (32 bytes = 256 bits).
	InvitationSecretBytes = 32

	// InvitationTokenExpiry é a validade padrão de um convite (24h).
	InvitationTokenExpiry = 24 * time.Hour

	// InvitationMinPasswordLength é o tamanho mínimo da senha do convidado.
	InvitationMinPasswordLength = 12
)

// alphabetInvitationPrefix é o alfabeto para o prefixo visível.
const alphabetInvitationPrefix = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var (
	// ErrInvitationInvalid indica token inexistente, expirado, usado ou revogado.
	ErrInvitationInvalid = errors.New("invalid or expired invitation token")

	// ErrInvitationWeakPassword indica senha abaixo do mínimo exigido.
	ErrInvitationWeakPassword = fmt.Errorf("password must be at least %d characters", InvitationMinPasswordLength)
)

// GenerateInvitationToken gera um token de convite criptograficamente seguro.
//
// Retorna o token raw (única vez), seu hash SHA-256 e o prefixo visível.
// O caller deve entregar o raw ao admin imediatamente e NUNCA persisti-lo.
func GenerateInvitationToken() (*InvitationTokenRaw, error) {
	prefix, err := randomInvitationPrefix()
	if err != nil {
		return nil, fmt.Errorf("generate invitation prefix: %w", err)
	}
	secret, err := randomInvitationSecret()
	if err != nil {
		return nil, fmt.Errorf("generate invitation secret: %w", err)
	}
	raw := InvitationPrefix + prefix + "-" + secret
	hash := hashInvitationToken(raw)
	return &InvitationTokenRaw{
		Raw:    raw,
		Hash:   hash,
		Prefix: prefix,
	}, nil
}

// ValidateInvitationPassword verifica se a senha atende ao mínimo exigido.
func ValidateInvitationPassword(password string) error {
	if len(password) < InvitationMinPasswordLength {
		return ErrInvitationWeakPassword
	}
	return nil
}

// InvitationTokenEquals compara um token raw com um hash em tempo constante.
func InvitationTokenEquals(rawToken, tokenHash string) bool {
	expected := hashInvitationToken(rawToken)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(tokenHash)) == 1
}

// IsExpired retorna true se o convite já expirou.
func (i *InvitationToken) IsExpired(now time.Time) bool {
	return now.After(i.ExpiresAt)
}

// CanBeUsed retorna true se o convite pode ser aceito no momento.
func (i *InvitationToken) CanBeUsed(now time.Time) bool {
	return i.Status == InvitationStatusPending && !i.IsExpired(now)
}

// hashInvitationToken calcula SHA-256 hex de um token raw.
func hashInvitationToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// randomInvitationPrefix gera InvitationPrefixLength caracteres alfanuméricos.
func randomInvitationPrefix() (string, error) {
	out := make([]byte, InvitationPrefixLength)
	max := byte(256 - (256 % len(alphabetInvitationPrefix)))
	for i := 0; i < InvitationPrefixLength; i++ {
		for {
			b := make([]byte, 1)
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
			if b[0] < max {
				out[i] = alphabetInvitationPrefix[int(b[0])%len(alphabetInvitationPrefix)]
				break
			}
		}
	}
	return string(out), nil
}

// randomInvitationSecret gera InvitationSecretBytes aleatórios em base64 URL-safe.
func randomInvitationSecret() (string, error) {
	b := make([]byte, InvitationSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
