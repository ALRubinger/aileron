package source

import (
	"context"
	"encoding/json"
	"log/slog"

	"golang.org/x/oauth2"
)

// PersistingTokenSource wraps an oauth2.TokenSource and calls a TokenSaver
// when a refresh produces a new token. This ensures rotated refresh tokens
// are written back to the vault so they survive across requests.
type PersistingTokenSource struct {
	base     oauth2.TokenSource
	original *oauth2.Token
	saver    TokenSaver
	ctx      context.Context
	saved    bool // prevents redundant saves within one Execute call
}

// NewPersistingTokenSource wraps base so that the first token refresh triggers
// a save via saver. The original token is used to detect whether a refresh
// actually occurred (by comparing access tokens).
func NewPersistingTokenSource(base oauth2.TokenSource, original *oauth2.Token, saver TokenSaver, ctx context.Context) *PersistingTokenSource {
	return &PersistingTokenSource{
		base:     base,
		original: original,
		saver:    saver,
		ctx:      ctx,
	}
}

// Token returns the current token, persisting it to the vault if a refresh
// occurred. Vault write failures are logged but do not fail the API call —
// the refreshed token is still valid in memory for this request.
func (p *PersistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if !p.saved && tok.AccessToken != p.original.AccessToken {
		tokenJSON, marshalErr := json.Marshal(tok)
		if marshalErr != nil {
			slog.Warn("failed to marshal refreshed token", "error", marshalErr)
			return tok, nil
		}
		if saveErr := p.saver(p.ctx, tokenJSON); saveErr != nil {
			slog.Warn("failed to persist refreshed token to vault", "error", saveErr)
		}
		p.saved = true
	}
	return tok, nil
}
