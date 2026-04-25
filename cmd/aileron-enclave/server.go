package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/source"
)

const enclaveSessionTTL = 30 * time.Minute

type enclaveServer struct {
	log            *slog.Logger
	registry       *connector.Registry
	sourceRegistry *source.Registry
	provider       string
	mu             sync.Mutex
	enclaveKey     *ecdh.PrivateKey
	sessionKey     []byte
	sessionID      string
	expiresAt      time.Time
	keks           *kekStore
	escrow         *escrowStore
}

func newEnclaveServer(log *slog.Logger, registry *connector.Registry, sourceReg *source.Registry, provider, dataDir string) (*enclaveServer, error) {
	escrow, err := newEscrowStore(dataDir)
	if err != nil {
		return nil, fmt.Errorf("initializing escrow store: %w", err)
	}
	return &enclaveServer{
		log:            log,
		registry:       registry,
		sourceRegistry: sourceReg,
		provider:       provider,
		keks:           newKEKStore(),
		escrow:         escrow,
	}, nil
}

func (s *enclaveServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /attest", s.handleAttest)
	mux.HandleFunc("POST /session", s.handleSession)
	mux.HandleFunc("POST /kek", s.handleTransmitKEK)
	mux.HandleFunc("POST /oauth/exchange", s.handleOAuthExchange)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /escrow", s.handleEscrowStore)
mux.HandleFunc("POST /escrow/list", s.handleEscrowList)
	mux.HandleFunc("POST /escrow/revoke", s.handleEscrowRevoke)
	mux.HandleFunc("POST /source/execute", s.handleSourceExecute)
	mux.HandleFunc("GET /health", s.handleHealth)
}

func (s *enclaveServer) handleAttest(w http.ResponseWriter, r *http.Request) {
	var req enclave.AttestationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Generate ephemeral ECDH key pair.
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generating key pair")
		return
	}

	s.mu.Lock()
	s.enclaveKey = priv
	s.mu.Unlock()

	// Get attestation token.
	token, err := fetchAttestationToken(s.provider, req.Audience)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "fetching attestation token: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, enclave.AttestationResponse{
		Token:     token,
		PublicKey: priv.PublicKey().Bytes(),
	})
}

func (s *enclaveServer) handleSession(w http.ResponseWriter, r *http.Request) {
	var req enclave.SessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.enclaveKey == nil {
		writeErr(w, http.StatusPreconditionFailed, "attestation required first")
		return
	}

	hostPub, err := ecdh.P256().NewPublicKey(req.PublicKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid public key: "+err.Error())
		return
	}

	raw, err := s.enclaveKey.ECDH(hostPub)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ECDH exchange failed")
		return
	}
	h := sha256.Sum256(raw)

	// Zero previous session key.
	zeroBytes(s.sessionKey)

	s.sessionKey = h[:]
	s.expiresAt = time.Now().Add(enclaveSessionTTL)

	b := make([]byte, 16)
	rand.Read(b)
	s.sessionID = hex.EncodeToString(b)

	writeJSON(w, http.StatusOK, enclave.SessionResponse{
		SessionID: s.sessionID,
		ExpiresAt: s.expiresAt.Format(time.RFC3339),
	})
}

func (s *enclaveServer) handleTransmitKEK(w http.ResponseWriter, r *http.Request) {
	var req enclave.TransmitKEKRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	sessionKey := copyBytes(s.sessionKey)
	s.mu.Unlock()

	if sessionKey == nil {
		writeErr(w, http.StatusPreconditionFailed, "no active session")
		return
	}
	defer zeroBytes(sessionKey)

	// Decrypt KEK using session key.
	kek, err := crypto.Decrypt(req.EncryptedKEK, sessionKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "KEK decryption failed: "+err.Error())
		return
	}

	s.keks.Store(req.UserID, kek)
	zeroBytes(kek)
	s.log.Info("KEK stored for user", "user_id", req.UserID)

	writeJSON(w, http.StatusOK, enclave.TransmitKEKResponse{Stored: true})
}

func (s *enclaveServer) handleOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var req enclave.OAuthExchangeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get user's KEK.
	kek, err := s.keks.Get(req.UserID)
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, "no KEK for user: "+err.Error())
		return
	}
	defer zeroBytes(kek)

	// Exchange authorization code for tokens.
	oauthResult, err := doOAuthExchange(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "OAuth exchange failed: "+err.Error())
		return
	}
	defer zeroBytes(oauthResult.tokenJSON)

	// Fetch user email.
	email, _ := doFetchEmail(r.Context(), req.UserInfoEndpoint, oauthResult.accessToken)

	// Encrypt token JSON with user's KEK.
	encrypted, err := crypto.Encrypt(oauthResult.tokenJSON, kek)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypting token: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, enclave.OAuthExchangeResponse{
		EncryptedToken: encrypted,
		Email:          email,
		TokenType:      oauthResult.tokenType,
		ExternalUserID: oauthResult.externalUserID,
		ExternalTeamID: oauthResult.externalTeamID,
	})
}

func (s *enclaveServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req enclave.ExecuteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve credential.
	var credential []byte
	if req.EscrowID != "" {
		cred, err := s.escrow.Get(req.EscrowID)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		credential = cred
	} else {
		// Decrypt credential with user's KEK.
		kek, err := s.keks.Get(req.UserID)
		if err != nil {
			writeErr(w, http.StatusPreconditionFailed, "no KEK for user: "+err.Error())
			return
		}
		plain, decErr := crypto.Decrypt(req.EncryptedCredential, kek)
		zeroBytes(kek)
		if decErr != nil {
			writeErr(w, http.StatusBadRequest, "decryption failed: "+decErr.Error())
			return
		}
		credential = plain
	}
	defer zeroBytes(credential)

	// Resolve connector.
	connType, connProvider := resolveConnectorID(req.ConnectorID)
	conn, ok := s.registry.Get(r.Context(), connType, connProvider)
	if !ok {
		writeErr(w, http.StatusBadRequest, "no connector for "+req.ConnectorID)
		return
	}

	// Execute.
	result, err := conn.Execute(r.Context(), connector.ExecutionRequest{
		GrantID:    req.GrantID,
		IntentID:   req.IntentID,
		ActionType: req.ActionType,
		Parameters: req.Parameters,
		Credential: &connector.InjectedCredential{
			Type:  req.CredentialType,
			Value: credential,
		},
	})
	if err != nil {
		writeJSON(w, http.StatusOK, enclave.ExecuteResponse{
			RequestID: req.RequestID,
			Status:    "failed",
			Error:     err.Error(),
		})
		return
	}

	status := "succeeded"
	if result.Status == connector.ExecutionStatusFailed {
		status = "failed"
	}

	writeJSON(w, http.StatusOK, enclave.ExecuteResponse{
		RequestID:  req.RequestID,
		Status:     status,
		Output:     result.Output,
		ReceiptRef: result.ReceiptRef,
		Error:      result.Error,
	})
}

func (s *enclaveServer) handleEscrowStore(w http.ResponseWriter, r *http.Request) {
	var req enclave.EscrowStoreRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Decrypt credential with user's KEK.
	kek, err := s.keks.Get(req.UserID)
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, "no KEK for user: "+err.Error())
		return
	}

	plaintext, err := crypto.Decrypt(req.EncryptedCredential, kek)
	zeroBytes(kek)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "decryption failed: "+err.Error())
		return
	}

	expiresAt, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		zeroBytes(plaintext)
		writeErr(w, http.StatusBadRequest, "invalid expires_at: "+err.Error())
		return
	}

	id := s.escrow.Store(req.GrantID, plaintext, req.CredentialType, req.ActionTypes, expiresAt)
	writeJSON(w, http.StatusOK, enclave.EscrowStoreResponse{EscrowID: id})
}

func (s *enclaveServer) handleEscrowList(w http.ResponseWriter, _ *http.Request) {
	entries := s.escrow.List()
	writeJSON(w, http.StatusOK, enclave.EscrowListResponse{Entries: entries})
}

func (s *enclaveServer) handleEscrowRevoke(w http.ResponseWriter, r *http.Request) {
	var req enclave.EscrowRevokeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.escrow.Revoke(req.EscrowID, req.GrantID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *enclaveServer) handleSourceExecute(w http.ResponseWriter, r *http.Request) {
	var req enclave.SourceExecuteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve credential from escrow — plaintext stays inside the enclave.
	credential, err := s.escrow.Get(req.EscrowID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	// Inject a TokenSaver that updates the escrowed credential when an
	// OAuth token is refreshed. This keeps the escrow fresh without
	// involving the host.
	escrowID := req.EscrowID
	ctx := source.WithTokenSaver(r.Context(), func(_ context.Context, newToken []byte) error {
		s.log.Info("updating escrowed credential after token refresh",
			"escrow_id", escrowID,
			"tool", req.Tool,
		)
		return s.escrow.Update(escrowID, newToken)
	})

	result, execErr := s.sourceRegistry.ExecuteTool(ctx, req.Tool, req.Params, credential)

	resp := enclave.SourceExecuteResponse{}
	if execErr != nil {
		resp.Error = execErr.Error()
	} else if result != nil {
		resultJSON, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			writeErr(w, http.StatusInternalServerError, "marshaling result: "+marshalErr.Error())
			return
		}
		resp.Result = resultJSON
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *enclaveServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	hasSession := s.sessionKey != nil && time.Now().Before(s.expiresAt)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"provider":       s.provider,
		"session_active": hasSession,
	})
}

// resolveConnectorID splits "type/provider" into its components.
func resolveConnectorID(id string) (string, string) {
	for i, c := range id {
		if c == '/' {
			return id[:i], id[i+1:]
		}
	}
	return id, ""
}

func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	defer r.Body.Close()
	return json.Unmarshal(body, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
