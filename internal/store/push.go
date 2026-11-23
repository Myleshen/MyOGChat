package store

// Web Push subscriptions (6.6). One row per browser push subscription; the
// browser-generated keys are not secret to the server (they encrypt payloads)
// but are never returned by the API.

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PushSubscription mirrors a row of the push_subscriptions table.
type PushSubscription struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Endpoint  string `json:"-"`
	P256dh    string `json:"-"`
	Auth      string `json:"-"`
	UserAgent string `json:"user_agent"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	Failures  int    `json:"failures"`
}

const pushSubscriptionCols = "id, user_id, endpoint, p256dh, auth, user_agent, created_at, last_used, failures"

// UpsertPushSubscription stores sub, updating the existing row when the same
// endpoint is already registered (a browser rotating its keys, or a different
// account signing in on the same device). It returns the row id.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub *PushSubscription) (string, error) {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, user_agent, created_at, last_used, failures)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0)
		 ON CONFLICT(endpoint) DO UPDATE SET
		   user_id = excluded.user_id,
		   p256dh = excluded.p256dh,
		   auth = excluded.auth,
		   user_agent = excluded.user_agent,
		   failures = 0`,
		sub.ID, sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth, sub.UserAgent, now); err != nil {
		return "", err
	}
	var id string
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM push_subscriptions WHERE endpoint = ?`, sub.Endpoint).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// ListPushSubscriptions returns a user's subscriptions, oldest first.
func (s *Store) ListPushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pushSubscriptionCols+` FROM push_subscriptions WHERE user_id = ? ORDER BY created_at`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var sub PushSubscription
		if err := rows.Scan(&sub.ID, &sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth,
			&sub.UserAgent, &sub.CreatedAt, &sub.LastUsed, &sub.Failures); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// CountPushSubscriptions returns how many subscriptions a user has.
func (s *Store) CountPushSubscriptions(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// DeletePushSubscription removes one of userID's subscriptions. It reports
// whether a row was deleted; another user's row is never touched.
func (s *Store) DeletePushSubscription(ctx context.Context, id, userID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeletePushSubscriptionByID removes a row without an owner check. Only the
// delivery path uses it (a push service said the subscription is gone).
func (s *Store) DeletePushSubscriptionByID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	return err
}

// PushSubscriptionOwner returns the owner of a subscription, or ErrNotFound.
func (s *Store) PushSubscriptionOwner(ctx context.Context, id string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM push_subscriptions WHERE id = ?`, id).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}

// MarkPushSent records a successful delivery: last_used moves and the failure
// streak resets.
func (s *Store) MarkPushSent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE push_subscriptions SET last_used = ?, failures = 0 WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return err
}

// MarkPushFailed records a failed delivery and returns the new consecutive
// failure count.
func (s *Store) MarkPushFailed(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE push_subscriptions SET last_used = ?, failures = failures + 1
		 WHERE id = ? RETURNING failures`,
		time.Now().UnixMilli(), id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}
