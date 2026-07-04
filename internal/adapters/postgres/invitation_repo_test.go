//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hacklabr/ai-pool/internal/domain"
	"github.com/hacklabr/ai-pool/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestInvitationRepo_Accept cria um convite e o consome, verificando que o
// usuário e o token API são criados e que o convite fica marcado como used.
func TestInvitationRepo_Accept(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Limpa schema.
	if err := execRaw(ctx, dsn, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("setup drop: %v", err)
	}
	if err := RunMigrations(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	repo := NewInvitationRepo(pool)
	pepper := []byte("test-pepper-32-bytes-long-ok!!")

	// Cria convite.
	raw, err := domain.GenerateInvitationToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	inv := &domain.InvitationToken{
		OrgID:       domain.DefaultOrganizationID,
		TokenHash:   raw.Hash,
		TokenPrefix: raw.Prefix,
		Username:    "invited.user",
		Role:        domain.RoleUser,
		Status:      domain.InvitationStatusPending,
		CreatedBy:   1,
		ExpiresAt:   time.Now().Add(domain.InvitationTokenExpiry),
	}
	id, err := repo.Create(ctx, inv)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	// Aceita convite.
	result, err := repo.Accept(ctx, raw.Hash, "secure-password-123", pepper)
	if err != nil {
		t.Fatalf("accept invitation: %v", err)
	}
	if result.Username != "invited.user" {
		t.Errorf("username = %q, want invited.user", result.Username)
	}
	if result.APITokenRaw == "" {
		t.Error("expected API token raw")
	}

	// Verifica que o convite está used.
	loaded, err := repo.GetByID(ctx, domain.DefaultOrganizationID, id)
	if err != nil {
		t.Fatalf("get invitation: %v", err)
	}
	if loaded.Status != domain.InvitationStatusUsed {
		t.Errorf("status = %q, want used", loaded.Status)
	}
	if loaded.UsedBy != result.UserID {
		t.Errorf("used_by = %d, want %d", loaded.UsedBy, result.UserID)
	}

	// Segunda tentativa deve falhar.
	_, err = repo.Accept(ctx, raw.Hash, "another-password-123", pepper)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound on second accept, got %v", err)
	}
}

// TestInvitationRepo_Accept_Expired rejeita convites expirados.
func TestInvitationRepo_Accept_Expired(t *testing.T) {
	dsn := os.Getenv("AIPP_TEST_DB")
	if dsn == "" {
		t.Skip("AIPP_TEST_DB não definido; pulando teste de integração")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := execRaw(ctx, dsn, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("setup drop: %v", err)
	}
	if err := RunMigrations(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	repo := NewInvitationRepo(pool)
	pepper := []byte("test-pepper-32-bytes-long-ok!!")

	raw, err := domain.GenerateInvitationToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	inv := &domain.InvitationToken{
		OrgID:       domain.DefaultOrganizationID,
		TokenHash:   raw.Hash,
		TokenPrefix: raw.Prefix,
		Username:    "expired.user",
		Role:        domain.RoleUser,
		Status:      domain.InvitationStatusPending,
		CreatedBy:   1,
		ExpiresAt:   time.Now().Add(-time.Hour),
	}
	if _, err := repo.Create(ctx, inv); err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	_, err = repo.Accept(ctx, raw.Hash, "secure-password-123", pepper)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for expired invitation, got %v", err)
	}
}
