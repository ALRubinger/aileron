// Package stripe implements the payments connector for Stripe.
//
// This connector executes payment.charge actions via the Stripe API using
// Stripe Issuing virtual cards. Credentials are injected at execution time
// from the vault and are never stored by this package.
package stripe

import (
	"context"
	"errors"

	"github.com/ALRubinger/aileron/core/connector"
)

const (
	connectorType     = "payments"
	connectorProvider = "stripe"
)

// Connector executes payment actions via Stripe.
type Connector struct{}

// New returns a new Stripe payments connector.
func New() *Connector {
	return &Connector{}
}

// Type implements connector.Connector.
func (c *Connector) Type() string { return connectorType }

// Provider implements connector.Connector.
func (c *Connector) Provider() string { return connectorProvider }

// Execute implements connector.Connector.
func (c *Connector) Execute(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if req.Credential == nil {
		return connector.ExecutionResult{}, errors.New("stripe: no credential injected")
	}

	switch req.ActionType {
	case "payment.charge":
		return c.charge(ctx, req)
	default:
		return connector.ExecutionResult{
			Status: connector.ExecutionStatusFailed,
			Error:  "stripe: unsupported action type: " + req.ActionType,
		}, nil
	}
}

func (c *Connector) charge(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement Stripe Issuing virtual card charge via Stripe API.
	// Steps:
	//   1. Resolve funding source to a Stripe Issuing cardholder / card pool.
	//   2. Create an ephemeral virtual card scoped to the approved parameters.
	//   3. Initiate the charge.
	//   4. Return the Stripe charge ID as ReceiptRef.
	_ = req
	return connector.ExecutionResult{}, errors.New("stripe: charge not yet implemented")
}
