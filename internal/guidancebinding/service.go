package guidancebinding

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Candidate is the currently effective Guidance contribution that a context
// creator wants to bind. Bind never mutates an existing consumer binding: the
// first revision remains pinned for that consumer's lifetime.
type Candidate struct {
	GuidanceID     string
	ProviderPlugin string
	Scope          string
	Revision       string
	Content        string
}

// Binding is durable metadata for one consumer/guidance pair. Content is kept
// in storage for exact delivery replay but is not part of ordinary metadata.
type Binding struct {
	RemoteSessionID string    `json:"remote_session_id"`
	ConsumerID      string    `json:"consumer_id"`
	GuidanceID      string    `json:"guidance_id"`
	ProviderPlugin  string    `json:"provider_plugin"`
	Scope           string    `json:"scope"`
	Revision        string    `json:"revision"`
	BoundAt         time.Time `json:"bound_at"`
}

// Delivery represents one binding attempt. Content is present only on the
// initial bind or an exact retry using the same non-empty delivery key.
type Delivery struct {
	Binding   Binding `json:"binding"`
	Delivered bool    `json:"delivered"`
	Content   string  `json:"content,omitempty"`
}

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (s *Service) Bind(ctx context.Context, remoteSessionID, consumerID, deliveryKey string, candidate Candidate) (Delivery, error) {
	if s == nil || s.db == nil {
		return Delivery{}, fmt.Errorf("guidance binding store unavailable")
	}
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	consumerID = strings.TrimSpace(consumerID)
	deliveryKey = strings.TrimSpace(deliveryKey)
	candidate.GuidanceID = strings.TrimSpace(candidate.GuidanceID)
	candidate.ProviderPlugin = strings.TrimSpace(candidate.ProviderPlugin)
	candidate.Scope = strings.TrimSpace(candidate.Scope)
	candidate.Revision = strings.TrimSpace(candidate.Revision)
	if remoteSessionID == "" || consumerID == "" {
		return Delivery{}, fmt.Errorf("remote session and consumer are required")
	}
	if candidate.GuidanceID == "" || candidate.ProviderPlugin == "" || candidate.Scope == "" || candidate.Revision == "" {
		return Delivery{}, fmt.Errorf("guidance id, provider, scope and revision are required")
	}
	if strings.TrimSpace(candidate.Content) == "" {
		return Delivery{}, fmt.Errorf("guidance %q content is empty", candidate.GuidanceID)
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO guidance_bindings
        (remote_session_id, consumer_id, guidance_id, provider_plugin, scope, revision, content, delivery_key, bound_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(remote_session_id, consumer_id, guidance_id) DO NOTHING`,
		remoteSessionID, consumerID, candidate.GuidanceID, candidate.ProviderPlugin, candidate.Scope,
		candidate.Revision, candidate.Content, deliveryKey, now.UnixMilli())
	if err != nil {
		return Delivery{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return Delivery{}, err
	}
	if inserted == 1 {
		if err := tx.Commit(); err != nil {
			return Delivery{}, err
		}
		return Delivery{
			Binding: Binding{
				RemoteSessionID: remoteSessionID, ConsumerID: consumerID, GuidanceID: candidate.GuidanceID,
				ProviderPlugin: candidate.ProviderPlugin, Scope: candidate.Scope, Revision: candidate.Revision, BoundAt: now,
			},
			Delivered: true,
			Content:   candidate.Content,
		}, nil
	}

	binding, content, storedDeliveryKey, err := readBinding(ctx, tx, remoteSessionID, consumerID, candidate.GuidanceID)
	if err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, err
	}
	deliver := deliveryKey != "" && deliveryKey == storedDeliveryKey
	delivery := Delivery{Binding: binding, Delivered: deliver}
	if deliver {
		delivery.Content = content
	}
	return delivery, nil
}

func (s *Service) ListConsumer(ctx context.Context, remoteSessionID, consumerID string) ([]Binding, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("guidance binding store unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT remote_session_id, consumer_id, guidance_id, provider_plugin, scope, revision, bound_at
        FROM guidance_bindings WHERE remote_session_id = ? AND consumer_id = ? ORDER BY bound_at, guidance_id`,
		strings.TrimSpace(remoteSessionID), strings.TrimSpace(consumerID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := []Binding{}
	for rows.Next() {
		var binding Binding
		var boundAt int64
		if err := rows.Scan(&binding.RemoteSessionID, &binding.ConsumerID, &binding.GuidanceID, &binding.ProviderPlugin, &binding.Scope, &binding.Revision, &boundAt); err != nil {
			return nil, err
		}
		binding.BoundAt = time.UnixMilli(boundAt).UTC()
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func readBinding(ctx context.Context, tx *sql.Tx, remoteSessionID, consumerID, guidanceID string) (Binding, string, string, error) {
	var (
		binding     Binding
		content     string
		deliveryKey string
		boundAt     int64
	)
	err := tx.QueryRowContext(ctx, `SELECT remote_session_id, consumer_id, guidance_id, provider_plugin, scope, revision, content, delivery_key, bound_at
        FROM guidance_bindings WHERE remote_session_id = ? AND consumer_id = ? AND guidance_id = ?`,
		remoteSessionID, consumerID, guidanceID).Scan(
		&binding.RemoteSessionID, &binding.ConsumerID, &binding.GuidanceID, &binding.ProviderPlugin,
		&binding.Scope, &binding.Revision, &content, &deliveryKey, &boundAt,
	)
	if err != nil {
		return Binding{}, "", "", err
	}
	binding.BoundAt = time.UnixMilli(boundAt).UTC()
	return binding, content, deliveryKey, nil
}
