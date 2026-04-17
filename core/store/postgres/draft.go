package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// DraftStore is a PostgreSQL implementation of store.DraftStore.
type DraftStore struct {
	db *DB
}

// NewDraftStore returns a PostgreSQL-backed draft store.
func NewDraftStore(db *DB) *DraftStore {
	return &DraftStore{db: db}
}

const draftColumns = `id, user_id, status, service, channel, author,
	message_body, message_ts, draft_body, sent_body, created_at, updated_at`

func (s *DraftStore) Create(ctx context.Context, d model.Draft) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO drafts
			(`+draftColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		d.ID, d.UserID, string(d.Status), d.Service, d.Channel, d.Author,
		d.MessageBody, d.MessageTS, d.DraftBody, d.SentBody,
		d.CreatedAt, d.UpdatedAt,
	)
	return err
}

func (s *DraftStore) Get(ctx context.Context, draftID string) (model.Draft, error) {
	return s.scanOne(ctx,
		`SELECT `+draftColumns+` FROM drafts WHERE id = $1`, draftID)
}

func (s *DraftStore) List(ctx context.Context, filter store.DraftFilter) ([]model.Draft, error) {
	query := `SELECT ` + draftColumns + ` FROM drafts WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.UserID != "" {
		query += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, string(*filter.Status))
		argIdx++
	}

	query += " ORDER BY created_at DESC"

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var drafts []model.Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	return drafts, rows.Err()
}

func (s *DraftStore) Update(ctx context.Context, d model.Draft) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE drafts
		 SET status=$2, service=$3, channel=$4, author=$5,
			 message_body=$6, message_ts=$7, draft_body=$8, sent_body=$9, updated_at=$10
		 WHERE id=$1`,
		d.ID, string(d.Status), d.Service, d.Channel, d.Author,
		d.MessageBody, d.MessageTS, d.DraftBody, d.SentBody, d.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "draft", ID: d.ID}
	}
	return nil
}

func (s *DraftStore) scanOne(ctx context.Context, query string, args ...any) (model.Draft, error) {
	row := s.db.Pool.QueryRow(ctx, query, args...)
	var d model.Draft
	var status string
	err := row.Scan(
		&d.ID, &d.UserID, &status, &d.Service, &d.Channel, &d.Author,
		&d.MessageBody, &d.MessageTS, &d.DraftBody, &d.SentBody,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Draft{}, &store.ErrNotFound{Entity: "draft", ID: fmt.Sprint(args...)}
		}
		return model.Draft{}, err
	}
	d.Status = model.DraftStatus(status)
	return d, nil
}

func scanDraft(rows pgx.Rows) (model.Draft, error) {
	var d model.Draft
	var status string
	err := rows.Scan(
		&d.ID, &d.UserID, &status, &d.Service, &d.Channel, &d.Author,
		&d.MessageBody, &d.MessageTS, &d.DraftBody, &d.SentBody,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return model.Draft{}, err
	}
	d.Status = model.DraftStatus(status)
	return d, nil
}
