package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rafael/ai-pool/internal/domain"
)

// KeyResolver implementa domain.KeyResolver usando os repositórios PostgreSQL.
//
// Implementação MÍNIMA de F3 (T13 será LRU; T14 afinidade; T15 failover):
//
//	ResolveKey(model) →
//	  1. modelRepo.GetActiveProviderForModel(model) → *Provider.
//	  2. keyRepo.ListUsableByProvider(provider.ID) → []ProviderKey.
//	  3. Retorna a PRIMEIRA chave usable (chaves com last_used_at NULL vêm
//	     primeiro via ORDER BY last_used_at NULLS FIRST no SQL — cold start
//	     distribui naturalmente).
//	  4. decryptor.Decrypt(key) → valor plaintext.
//
// F5 substituirá este resolver por uma versão que combina LRUStore,
// AffinityStore e FailoverChain. A interface domain.KeyResolver permanece
// estável.
type KeyResolver struct {
	modelRepo *ModelRepo
	keyRepo   *KeyRepo
	decryptor domain.KeyDecryptor
}

// NewKeyResolver cria o resolver.
func NewKeyResolver(pool *pgxpool.Pool, decryptor domain.KeyDecryptor) *KeyResolver {
	return &KeyResolver{
		modelRepo: NewModelRepo(pool),
		keyRepo:   NewKeyRepo(pool),
		decryptor: decryptor,
	}
}

// ResolveKey implementa domain.KeyResolver.
func (r *KeyResolver) ResolveKey(ctx context.Context, orgID domain.OrganizationID, model string) (*domain.KeyResolution, error) {
	if model == "" {
		return nil, fmt.Errorf("key resolver: empty model")
	}

	// 1. Provider que oferece este model (first-match entre providers ativos).
	provider, err := r.modelRepo.GetActiveProviderForModel(ctx, orgID, model)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, domain.ErrModelNotFound
		}
		return nil, fmt.Errorf("key resolver: provider lookup: %w", err)
	}

	// 2. Lista de chaves usable do provider.
	keys, err := r.keyRepo.ListUsableByProvider(ctx, orgID, provider.ID)
	if err != nil {
		return nil, fmt.Errorf("key resolver: keys lookup: %w", err)
	}
	if len(keys) == 0 {
		// Provider existe mas não há chaves 'enabled'. Em F5, isto dispara
		// auto-disable check do provider; por ora, erro descritivo.
		return nil, domain.ErrNoKeysAvailable
	}

	// 3. Primeira chave (cold-start NULLS FIRST no SQL → chaves nunca usadas
	// vêm primeiro). F5: PickLeastUsed do LRUStore + afinidade.
	key := keys[0]

	// 4. Decifra o valor. Em dev (IdentityDecryptor) é passthrough; em prod
	// (F7, AESGCMDecryptor) aplica envelope encryption.
	decrypted, err := r.decryptor.Decrypt(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("key resolver: decrypt: %w", err)
	}

	return &domain.KeyResolution{
		Provider:       *provider,
		Key:            key,
		DecryptedValue: decrypted,
	}, nil
}
