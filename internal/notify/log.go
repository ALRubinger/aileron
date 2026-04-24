package notify

import (
	"context"
	"log/slog"
)

// LogNotifier logs approval notifications to slog. For development only.
type LogNotifier struct {
	log *slog.Logger
}

// NewLogNotifier returns a Notifier that logs to the provided logger.
func NewLogNotifier(log *slog.Logger) *LogNotifier {
	return &LogNotifier{log: log}
}

func (n *LogNotifier) Notify(_ context.Context, notif Notification) error {
	n.log.Info("approval notification",
		"approval_id", notif.ApprovalID,
		"intent_id", notif.IntentID,
		"summary", notif.Summary,
		"review_url", notif.ReviewURL,
		"approvers", len(notif.Approvers),
	)
	return nil
}
