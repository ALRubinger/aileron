package app

import (
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/config"
	"github.com/ALRubinger/aileron/core/source"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func TestRegisterProviders_GoogleSigninOnly(t *testing.T) {
	cfg := &config.AuthConfig{
		GoogleSigninClientID:     "signin-id",
		GoogleSigninClientSecret: "signin-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("google"); !ok {
		t.Error("expected Google auth provider registered")
	}
	if len(accountReg.Providers()) != 0 {
		t.Errorf("expected no account providers, got %v", accountReg.Providers())
	}
}

func TestRegisterProviders_GoogleConnectorOnly(t *testing.T) {
	cfg := &config.AuthConfig{
		GoogleConnectorClientID:     "connector-id",
		GoogleConnectorClientSecret: "connector-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("google"); ok {
		t.Error("expected no Google auth provider when only connector is configured")
	}
	providers := accountReg.Providers()
	if len(providers) == 0 {
		t.Fatal("expected account providers registered")
	}
	found := false
	for _, p := range providers {
		if p == "gmail" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected gmail in account providers, got %v", providers)
	}
	// Source connectors should include gmail and calendar.
	if _, ok := sourceReg.Get("gmail"); !ok {
		t.Error("expected gmail source connector")
	}
	if _, ok := sourceReg.Get("google_calendar"); !ok {
		t.Error("expected calendar source connector")
	}
}

func TestRegisterProviders_GoogleBothSigninAndConnector(t *testing.T) {
	cfg := &config.AuthConfig{
		GoogleSigninClientID:        "signin-id",
		GoogleSigninClientSecret:    "signin-secret",
		GoogleConnectorClientID:     "connector-id",
		GoogleConnectorClientSecret: "connector-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("google"); !ok {
		t.Error("expected Google auth provider")
	}
	if len(accountReg.Providers()) == 0 {
		t.Error("expected account providers")
	}
}

func TestRegisterProviders_GitHubSigninOnly(t *testing.T) {
	cfg := &config.AuthConfig{
		GitHubSigninClientID:     "signin-id",
		GitHubSigninClientSecret: "signin-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("github"); !ok {
		t.Error("expected GitHub auth provider registered")
	}
	if len(accountReg.Providers()) != 0 {
		t.Errorf("expected no account providers, got %v", accountReg.Providers())
	}
}

func TestRegisterProviders_GitHubConnectorOnly(t *testing.T) {
	cfg := &config.AuthConfig{
		GitHubConnectorClientID:     "connector-id",
		GitHubConnectorClientSecret: "connector-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("github"); ok {
		t.Error("expected no GitHub auth provider when only connector is configured")
	}
	providers := accountReg.Providers()
	found := false
	for _, p := range providers {
		if p == "github_repos" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected github_repos in account providers, got %v", providers)
	}
	if _, ok := sourceReg.Get("github_repos"); !ok {
		t.Error("expected github source connector")
	}
}

func TestRegisterProviders_GitHubBothSigninAndConnector(t *testing.T) {
	cfg := &config.AuthConfig{
		GitHubSigninClientID:        "signin-id",
		GitHubSigninClientSecret:    "signin-secret",
		GitHubConnectorClientID:     "connector-id",
		GitHubConnectorClientSecret: "connector-secret",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("github"); !ok {
		t.Error("expected GitHub auth provider")
	}
	if len(accountReg.Providers()) == 0 {
		t.Error("expected account providers")
	}
}

func TestRegisterProviders_NoneConfigured(t *testing.T) {
	cfg := &config.AuthConfig{}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	if _, ok := authReg.Get("google"); ok {
		t.Error("expected no Google auth provider")
	}
	if _, ok := authReg.Get("github"); ok {
		t.Error("expected no GitHub auth provider")
	}
	if len(accountReg.Providers()) != 0 {
		t.Errorf("expected no account providers, got %v", accountReg.Providers())
	}
}

func TestRegisterProviders_SlackConnector(t *testing.T) {
	cfg := &config.AuthConfig{
		SlackClientID:      "slack-id",
		SlackClientSecret:  "slack-secret",
		SlackSigningSecret: "slack-signing",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	providers := accountReg.Providers()
	found := false
	for _, p := range providers {
		if p == "slack" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack in account providers, got %v", providers)
	}
	if _, ok := sourceReg.Get("slack"); !ok {
		t.Error("expected slack source connector")
	}
}

func TestRegisterProviders_AllProviders(t *testing.T) {
	cfg := &config.AuthConfig{
		GoogleSigninClientID:        "gs-id",
		GoogleSigninClientSecret:    "gs-secret",
		GoogleConnectorClientID:     "gc-id",
		GoogleConnectorClientSecret: "gc-secret",
		GitHubSigninClientID:        "ghs-id",
		GitHubSigninClientSecret:    "ghs-secret",
		GitHubConnectorClientID:     "ghc-id",
		GitHubConnectorClientSecret: "ghc-secret",
		SlackClientID:               "slack-id",
		SlackClientSecret:           "slack-secret",
		SlackSigningSecret:          "slack-signing",
	}
	authReg := auth.NewRegistry()
	accountReg := account.NewRegistry()
	sourceReg := source.NewRegistry()

	registerProviders(cfg, authReg, accountReg, sourceReg, mem.NewConnectedAccountStore(), vault.NewMemVault(), slog.Default())

	// Auth: google + github
	if _, ok := authReg.Get("google"); !ok {
		t.Error("expected Google auth provider")
	}
	if _, ok := authReg.Get("github"); !ok {
		t.Error("expected GitHub auth provider")
	}

	// Account providers: gmail, google_calendar, slack, github_repos
	providers := accountReg.Providers()
	if len(providers) < 3 {
		t.Errorf("expected at least 3 account providers, got %v", providers)
	}

	// Source connectors: gmail, calendar, slack, github
	for _, name := range []string{"gmail", "google_calendar", "slack", "github_repos"} {
		if _, ok := sourceReg.Get(name); !ok {
			t.Errorf("expected %s source connector", name)
		}
	}
}
