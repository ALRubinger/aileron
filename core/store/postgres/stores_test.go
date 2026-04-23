//go:build integration

package postgres_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/store/pgtest"
	"github.com/ALRubinger/aileron/core/store/postgres"
	"github.com/ALRubinger/aileron/core/store/storetest"
)

func TestStores(t *testing.T) {
	db := pgtest.New(t, pgtest.FullSchema)

	h := &storetest.Harness{
		Enterprises:       postgres.NewEnterpriseStore(db),
		Users:             postgres.NewUserStore(db),
		UserAuthProviders: postgres.NewUserAuthProviderStore(db),
		Sessions:          postgres.NewSessionStore(db),
		VerificationCodes: postgres.NewVerificationCodeStore(db),
		SSOConfigs:        postgres.NewSSOConfigStore(db),
		ConnectedAccounts: postgres.NewConnectedAccountStore(db),
		Drafts:            postgres.NewDraftStore(db),
		DraftFeedback:     postgres.NewDraftFeedbackStore(db),
		UserInstructions:  postgres.NewUserInstructionStore(db),
		UserKeyMaterials:  postgres.NewUserKeyMaterialStore(db),
		LLMConfigs:        postgres.NewLLMConfigStore(db),
		EscrowIndex:       postgres.NewEscrowIndexStore(db),
	}

	t.Run("Enterprise", func(t *testing.T) { storetest.TestEnterpriseStore(t, h) })
	t.Run("User", func(t *testing.T) { storetest.TestUserStore(t, h) })
	t.Run("UserAuthProvider", func(t *testing.T) { storetest.TestUserAuthProviderStore(t, h) })
	t.Run("Session", func(t *testing.T) { storetest.TestSessionStore(t, h) })
	t.Run("VerificationCode", func(t *testing.T) { storetest.TestVerificationCodeStore(t, h) })
	t.Run("SSOConfig", func(t *testing.T) { storetest.TestSSOConfigStore(t, h) })
	t.Run("ConnectedAccount", func(t *testing.T) { storetest.TestConnectedAccountStore(t, h) })
	t.Run("Draft", func(t *testing.T) { storetest.TestDraftStore(t, h) })
	t.Run("DraftFeedback", func(t *testing.T) { storetest.TestDraftFeedbackStore(t, h) })
	t.Run("UserInstruction", func(t *testing.T) { storetest.TestUserInstructionStore(t, h) })
	t.Run("UserKeyMaterial", func(t *testing.T) { storetest.TestUserKeyMaterialStore(t, h) })
	t.Run("LLMConfig", func(t *testing.T) { storetest.TestLLMConfigStore(t, h) })
	t.Run("EscrowIndex", func(t *testing.T) { storetest.TestEscrowIndexStore(t, h) })
}
