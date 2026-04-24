package config

import "os"

// TEEConfig holds Trusted Execution Environment configuration, loaded from
// environment variables.
type TEEConfig struct {
	// Provider selects the TEE backend.
	// Env: AILERON_TEE_PROVIDER (values: "", "local", "confidential-space")
	Provider string

	// EnclaveURL is the base URL of the enclave binary (for remote providers).
	// Env: AILERON_ENCLAVE_URL
	EnclaveURL string

	// ImageDigest is the expected container image digest for attestation.
	// Env: AILERON_ENCLAVE_IMAGE_DIGEST
	ImageDigest string

	// ProjectID is the expected GCP project ID for attestation.
	// Env: AILERON_GCP_PROJECT_ID
	ProjectID string
}

// LoadTEEConfig reads TEE configuration from environment variables.
func LoadTEEConfig() *TEEConfig {
	return &TEEConfig{
		Provider:    os.Getenv("AILERON_TEE_PROVIDER"),
		EnclaveURL:  os.Getenv("AILERON_ENCLAVE_URL"),
		ImageDigest: os.Getenv("AILERON_ENCLAVE_IMAGE_DIGEST"),
		ProjectID:   os.Getenv("AILERON_GCP_PROJECT_ID"),
	}
}

// TEEEnabled returns true when a TEE provider is configured.
func (c *TEEConfig) TEEEnabled() bool {
	return c.Provider != ""
}
