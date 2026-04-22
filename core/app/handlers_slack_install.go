package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/vault"
)

// slackInstallResponse is the HTML page shown after a successful bot installation.
const slackInstallSuccessHTML = `<!DOCTYPE html>
<html><head><title>Aileron — Installed</title></head>
<body style="font-family:system-ui;display:flex;justify-content:center;align-items:center;height:100vh;margin:0">
<div style="text-align:center">
<h2>Aileron has been installed in your Slack workspace</h2>
<p>You can close this tab.</p>
</div>
</body></html>`

// slackBotInstallResponse is the Slack oauth.v2.access response when an admin
// installs the app. The top-level access_token is the bot token.
type slackBotInstallResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	AccessToken string `json:"access_token"` // bot token (xoxb-...)
	TokenType   string `json:"token_type"`
	BotUserID   string `json:"bot_user_id"`
	Team        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
}

// slackBotTokenExchanger exchanges a code for a bot install response.
type slackBotTokenExchanger func(clientID, clientSecret, code, redirectURI string) (*slackBotInstallResponse, error)

// handleSlackInstall handles GET /v1/slack/install/callback.
// This is the OAuth redirect endpoint for Slack bot installation. When a
// workspace admin installs the Aileron app from the Slack App Directory,
// Slack redirects here with an authorization code. We exchange it for a bot
// token and store it in the system vault.
//
// This endpoint is unauthenticated — the admin may not have an Aileron account.
func (s *apiServer) handleSlackInstall(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	if s.systemVault == nil {
		s.log.Error("slack install: system vault not configured")
		http.Error(w, "system vault not configured", http.StatusInternalServerError)
		return
	}

	exchanger := s.slackBotExchanger
	if exchanger == nil {
		exchanger = s.defaultSlackBotTokenExchange
	}

	redirectURI := buildRedirectURI(r, "/v1/slack/install/callback")
	resp, err := exchanger(s.slackClientID, s.slackClientSecret, code, redirectURI)
	if err != nil {
		s.log.Error("slack install: token exchange failed", "error", err)
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	if resp.AccessToken == "" {
		s.log.Error("slack install: no bot token in response")
		http.Error(w, "no bot token returned by Slack", http.StatusBadGateway)
		return
	}

	teamID := resp.Team.ID
	if teamID == "" {
		s.log.Error("slack install: no team ID in response")
		http.Error(w, "no team ID returned by Slack", http.StatusBadGateway)
		return
	}

	// Store the bot token in the system vault (encrypted at rest).
	path := account.SlackBotTokenVaultPath(teamID)
	if err := s.systemVault.Put(r.Context(), path, []byte(resp.AccessToken), vault.Metadata{
		Type: "slack_bot_token",
		Labels: map[string]string{
			"team_id":    teamID,
			"team_name":  resp.Team.Name,
			"bot_user_id": resp.BotUserID,
		},
	}); err != nil {
		s.log.Error("slack install: failed to store bot token", "error", err, "team_id", teamID)
		http.Error(w, "failed to store bot token", http.StatusInternalServerError)
		return
	}

	s.log.Info("slack bot installed",
		"team_id", teamID,
		"team_name", resp.Team.Name,
		"bot_user_id", resp.BotUserID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, slackInstallSuccessHTML)
}

// defaultSlackBotTokenExchange performs the Slack OAuth v2 token exchange for
// bot installation. The top-level access_token in the response is the bot token.
func (s *apiServer) getSlackTokenURL() string {
	if s.slackTokenURL != "" {
		return s.slackTokenURL
	}
	return slackTokenURL
}

func (s *apiServer) defaultSlackBotTokenExchange(clientID, clientSecret, code, redirectURI string) (*slackBotInstallResponse, error) {
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	resp, err := http.PostForm(s.getSlackTokenURL(), data)
	if err != nil {
		return nil, fmt.Errorf("posting to token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	var oauthResp slackBotInstallResponse
	if err := json.Unmarshal(body, &oauthResp); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}

	if !oauthResp.OK {
		return nil, fmt.Errorf("slack oauth error: %s", oauthResp.Error)
	}

	return &oauthResp, nil
}

// slackTokenURL is the Slack OAuth v2 token exchange endpoint.
const slackTokenURL = "https://slack.com/api/oauth.v2.access"

// buildRedirectURI reconstructs the redirect URI from the incoming request.
func buildRedirectURI(r *http.Request, path string) string {
	scheme := "https"
	if r.TLS == nil {
		if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
			scheme = fwd
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + path
}
