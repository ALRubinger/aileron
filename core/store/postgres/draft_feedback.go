package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// DraftFeedbackStore is a PostgreSQL implementation of store.DraftFeedbackStore.
type DraftFeedbackStore struct {
	db *DB
}

// NewDraftFeedbackStore returns a PostgreSQL-backed draft feedback store.
func NewDraftFeedbackStore(db *DB) *DraftFeedbackStore {
	return &DraftFeedbackStore{db: db}
}

const draftFeedbackColumns = `id, user_id, draft_id, signal, service, channel,
	draft_body, sent_body, tools_used, created_at`

func (s *DraftFeedbackStore) Create(ctx context.Context, f model.DraftFeedback) error {
	tools, _ := json.Marshal(f.ToolsUsed)
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO draft_feedback
			(`+draftFeedbackColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		f.ID, f.UserID, f.DraftID, string(f.Signal), f.Service, f.Channel,
		f.DraftBody, f.SentBody, string(tools), f.CreatedAt,
	)
	return err
}

func (s *DraftFeedbackStore) List(ctx context.Context, filter store.DraftFeedbackFilter) ([]model.DraftFeedback, error) {
	query := `SELECT ` + draftFeedbackColumns + ` FROM draft_feedback WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.UserID != "" {
		query += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Signal != nil {
		query += fmt.Sprintf(" AND signal = $%d", argIdx)
		args = append(args, string(*filter.Signal))
		argIdx++
	}

	query += " ORDER BY created_at DESC"

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feedbacks []model.DraftFeedback
	for rows.Next() {
		f, err := scanDraftFeedback(rows)
		if err != nil {
			return nil, err
		}
		feedbacks = append(feedbacks, f)
	}
	return feedbacks, rows.Err()
}

func scanDraftFeedback(rows pgx.Rows) (model.DraftFeedback, error) {
	var f model.DraftFeedback
	var signal, toolsJSON string
	err := rows.Scan(
		&f.ID, &f.UserID, &f.DraftID, &signal, &f.Service, &f.Channel,
		&f.DraftBody, &f.SentBody, &toolsJSON, &f.CreatedAt,
	)
	if err != nil {
		return model.DraftFeedback{}, err
	}
	f.Signal = model.FeedbackSignal(signal)
	_ = json.Unmarshal([]byte(toolsJSON), &f.ToolsUsed)
	return f, nil
}
