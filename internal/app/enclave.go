package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/enclave/gcs"
	"github.com/ALRubinger/aileron/internal/enclave/local"
)

// newEnclaveClient creates the appropriate enclave client and verifier based
// on the TEE configuration. Returns (nil, nil, nil) when TEE is not enabled.
func newEnclaveClient(cfg *config.TEEConfig, log *slog.Logger, registry *connector.Registry) (enclave.Client, enclave.Verifier, error) {
	if !cfg.TEEEnabled() {
		return nil, nil, nil
	}

	switch cfg.Provider {
	case "local":
		log.Info("TEE provider: local (dev/test, no hardware isolation)")
		executeFn := buildLocalExecuteFn(registry)
		return local.New(executeFn), &local.DevVerifier{}, nil

	case "confidential-space":
		if cfg.EnclaveURL == "" {
			return nil, nil, fmt.Errorf("AILERON_ENCLAVE_URL required for confidential-space provider")
		}
		if cfg.ImageDigest == "" {
			return nil, nil, fmt.Errorf("AILERON_ENCLAVE_IMAGE_DIGEST required for confidential-space provider")
		}
		if cfg.ProjectID == "" {
			return nil, nil, fmt.Errorf("AILERON_GCP_PROJECT_ID required for confidential-space provider")
		}
		log.Info("TEE provider: Google Confidential Space", "url", cfg.EnclaveURL)
		client := gcs.New(gcs.Config{BaseURL: cfg.EnclaveURL})
		verifier := &gcs.Verifier{
			ExpectedImageDigest: cfg.ImageDigest,
			ExpectedProjectID:   cfg.ProjectID,
		}
		return client, verifier, nil

	default:
		return nil, nil, fmt.Errorf("unknown TEE provider: %q", cfg.Provider)
	}
}

// buildLocalExecuteFn creates the ExecuteFn for the local enclave client.
// It resolves the connector from the registry and executes it.
func buildLocalExecuteFn(registry *connector.Registry) local.ExecuteFn {
	return func(ctx context.Context, req enclave.ExecuteRequest, credential []byte) (enclave.ExecuteResponse, error) {
		connType, connProvider := resolveConnector(req.ActionType)
		conn, ok := registry.Get(ctx, connType, connProvider)
		if !ok {
			return enclave.ExecuteResponse{
				RequestID: req.RequestID,
				Status:    "failed",
				Error:     "no connector for " + req.ActionType,
			}, nil
		}

		result, err := conn.Execute(ctx, connector.ExecutionRequest{
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
			return enclave.ExecuteResponse{
				RequestID: req.RequestID,
				Status:    "failed",
				Error:     err.Error(),
			}, nil
		}

		status := "succeeded"
		if result.Status == connector.ExecutionStatusFailed {
			status = "failed"
		}
		return enclave.ExecuteResponse{
			RequestID:  req.RequestID,
			Status:     status,
			Output:     result.Output,
			ReceiptRef: result.ReceiptRef,
			Error:      result.Error,
		}, nil
	}
}
