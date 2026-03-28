// Package notify defines the SPI for human approval notifications.
//
// When an intent requires human approval, the approval orchestrator
// dispatches a notification via this SPI. The notification includes a
// deep link back to the web app where the approver can review and act.
//
// The built-in implementations deliver via Slack and email. Alternative
// implementations can target other channels (PagerDuty, SMS, Teams, etc.)
// without modifying the orchestrator.
package notify

import "context"

// Notifier delivers approval request notifications to human approvers.
type Notifier interface {
	// Notify sends an approval request notification to the listed approvers.
	Notify(ctx context.Context, n Notification) error
}

// Notification is the data delivered to approvers when an approval is needed.
type Notification struct {
	ApprovalID  string
	IntentID    string
	WorkspaceID string
	// Summary is a one-line human-readable description of what the agent
	// wants to do, drawn from the intent.
	Summary string
	// ReviewURL is the deep link to the approval UI for this request.
	ReviewURL string
	// Approvers are the people who should receive this notification.
	Approvers []Approver
}

// Approver is a person who should be notified about an approval request.
type Approver struct {
	PrincipalID string
	DisplayName string
	// Contact is the channel-specific address (email address, Slack user ID).
	Contact string
}

// Multi dispatches a notification to multiple notifiers in sequence.
// Errors from individual notifiers are collected but do not stop delivery
// to subsequent notifiers.
type Multi struct {
	notifiers []Notifier
}

// NewMulti returns a Notifier that dispatches to all provided notifiers.
func NewMulti(notifiers ...Notifier) *Multi {
	return &Multi{notifiers: notifiers}
}

// Notify dispatches the notification to all registered notifiers.
func (m *Multi) Notify(ctx context.Context, n Notification) error {
	var errs []error
	for _, notifier := range m.notifiers {
		if err := notifier.Notify(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return &MultiError{Errors: errs}
	}
	return nil
}

// MultiError collects errors from multiple notifiers.
type MultiError struct {
	Errors []error
}

func (e *MultiError) Error() string {
	msg := "notification errors:"
	for _, err := range e.Errors {
		msg += " " + err.Error() + ";"
	}
	return msg
}
