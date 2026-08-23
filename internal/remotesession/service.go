package remotesession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mcpx/internal/auth"
)

var (
	ErrNotFound     = errors.New("remote session not found")
	ErrForbidden    = errors.New("remote session access denied")
	ErrConflict     = errors.New("remote session version conflict")
	ErrInvalidToken = errors.New("invalid or expired handoff token")
	ErrInvalidInput = errors.New("invalid remote session request")
)

type Service struct {
	db         *sql.DB
	now        func() time.Time
	observer   EventObserver
	observerMu sync.RWMutex
}

// EventObserver receives a committed Remote Session event. Observers must
// treat the callback as best-effort and must not mutate the business event.
type EventObserver func(Session, Event)

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// SetEventObserver configures an optional lifecycle bridge for read-only
// observers. The callback runs after the business transaction has committed.
func (s *Service) SetEventObserver(observer EventObserver) {
	s.observerMu.Lock()
	s.observer = observer
	s.observerMu.Unlock()
}

func (s *Service) notifyEvent(session Session, event Event) {
	s.observerMu.RLock()
	observer := s.observer
	s.observerMu.RUnlock()
	if observer != nil {
		observer(session, event)
	}
}

type Session struct {
	ID                    string     `json:"remote_session_id"`
	WorkspaceID           string     `json:"workspace_id"`
	WorkspaceName         string     `json:"workspace"`
	WorkspacePath         string     `json:"-"`
	Label                 string     `json:"label"`
	Description           string     `json:"description"`
	Status                string     `json:"status"`
	OwnerPrincipalID      string     `json:"-"`
	Role                  string     `json:"role,omitempty"`
	BaseGitHead           string     `json:"base_git_head,omitempty"`
	BaseTreeDigest        string     `json:"base_tree_digest,omitempty"`
	EnvironmentSnapshotID string     `json:"environment_snapshot_id,omitempty"`
	Version               int        `json:"version"`
	CreatedAt             time.Time  `json:"created_at"`
	LastActiveAt          time.Time  `json:"last_active_at"`
	ClosedAt              *time.Time `json:"closed_at,omitempty"`
}

type CreateInput struct {
	WorkspaceID     string
	WorkspaceName   string
	WorkspacePath   string
	Label           string
	Description     string
	BaseGitHead     string
	BaseTreeDigest  string
	ClientRequestID string
	ClientName      string
	ClientVersion   string
}

type Attachment struct {
	ID              string    `json:"attachment_id"`
	RemoteSessionID string    `json:"remote_session_id"`
	ClientName      string    `json:"client_name,omitempty"`
	ClientVersion   string    `json:"client_version,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type AttachmentInput struct {
	ClientRequestID string
	ClientName      string
	ClientVersion   string
}

type CreateResult struct {
	Session                  Session    `json:"session"`
	Attachment               Attachment `json:"attachment"`
	ResumeToken              string     `json:"resume_token,omitempty"`
	ExpiresAt                time.Time  `json:"resume_token_expires_at"`
	ResumeTokenAlreadyIssued bool       `json:"resume_token_already_issued,omitempty"`
	EnvironmentSnapshotID    string     `json:"environment_snapshot_id,omitempty"`
	EnvironmentStaticDigest  string     `json:"environment_static_digest,omitempty"`
}

type AttachResult struct {
	Session    Session    `json:"session"`
	Attachment Attachment `json:"attachment"`
}

type ListInput struct {
	WorkspaceID string
	Workspace   string
	Statuses    []string
	Query       string
	Limit       int
	Cursor      string
}

type ListResult struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type Event struct {
	Sequence        int64          `json:"sequence"`
	RemoteSessionID string         `json:"remote_session_id"`
	PrincipalID     string         `json:"-"`
	ClientName      string         `json:"client_name,omitempty"`
	Type            string         `json:"type"`
	OperationID     string         `json:"operation_id,omitempty"`
	Summary         string         `json:"summary"`
	ResourceURI     string         `json:"resource_uri,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

type EventsInput struct {
	Types         []string
	AfterSequence int64
	Limit         int
}

type HandoffResult struct {
	HandoffToken string    `json:"handoff_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Role         string    `json:"role"`
}

func (s *Service) Create(ctx context.Context, principal auth.Principal, in CreateInput) (CreateResult, error) {
	if strings.TrimSpace(in.WorkspaceName) == "" || strings.TrimSpace(in.WorkspacePath) == "" {
		return CreateResult{}, fmt.Errorf("%w: workspace required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Label) == "" {
		in.Label = "Remote development session"
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback()
	if err := upsertPrincipal(ctx, tx, principal, now); err != nil {
		return CreateResult{}, err
	}
	if in.ClientRequestID != "" {
		var cached string
		err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_records
            WHERE remote_session_id = '' AND principal_id = ? AND client_request_id = ? AND operation = 'remote_session_create' AND expires_at > ?`,
			principal.ID, in.ClientRequestID, now.UnixMilli()).Scan(&cached)
		if err == nil {
			var result CreateResult
			if json.Unmarshal([]byte(cached), &result) == nil {
				result.ResumeToken = ""
				result.ResumeTokenAlreadyIssued = true
				return result, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, err
		}
	}

	sessionUUID, err := uuid.NewRandom()
	if err != nil {
		return CreateResult{}, err
	}
	sessionID := sessionUUID.String()
	handoffID, err := randomID("rsh_", 12)
	if err != nil {
		return CreateResult{}, err
	}
	resumeToken, err := randomToken("rsrt_", 32)
	if err != nil {
		return CreateResult{}, err
	}
	expiresAt := now.Add(24 * time.Hour)
	session := Session{
		ID: sessionID, WorkspaceID: strings.TrimSpace(in.WorkspaceID), WorkspaceName: in.WorkspaceName, WorkspacePath: in.WorkspacePath,
		Label: in.Label, Description: in.Description, Status: "active",
		OwnerPrincipalID: principal.ID, Role: "owner", BaseGitHead: in.BaseGitHead,
		BaseTreeDigest: in.BaseTreeDigest, Version: 1, CreatedAt: now, LastActiveAt: now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_sessions
        (id, workspace_id, workspace_name, workspace_path, label, description, status, owner_principal_id,
         base_git_head, base_tree_digest, version, created_at, last_active_at)
        VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, 1, ?, ?)`,
		session.ID, session.WorkspaceID, session.WorkspaceName, session.WorkspacePath, session.Label, session.Description,
		principal.ID, nullable(session.BaseGitHead), nullable(session.BaseTreeDigest), now.UnixMilli(), now.UnixMilli()); err != nil {
		return CreateResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_session_members
        (remote_session_id, principal_id, role, joined_at, last_active_at) VALUES (?, ?, 'owner', ?, ?)`,
		session.ID, principal.ID, now.UnixMilli(), now.UnixMilli()); err != nil {
		return CreateResult{}, err
	}
	if err := recordClientTx(ctx, tx, session.ID, principal.ID, in.ClientName, in.ClientVersion, now); err != nil {
		return CreateResult{}, err
	}
	attachment, err := createAttachmentTx(ctx, tx, session.ID, principal.ID, in.ClientName, in.ClientVersion, now)
	if err != nil {
		return CreateResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_session_handoffs
        (id, remote_session_id, token_hash, role, created_by, note, created_at, expires_at)
        VALUES (?, ?, ?, 'editor', ?, 'initial resume token', ?, ?)`,
		handoffID, session.ID, tokenDigest(resumeToken), principal.ID, now.UnixMilli(), expiresAt.UnixMilli()); err != nil {
		return CreateResult{}, err
	}
	createdEvent := Event{RemoteSessionID: session.ID, PrincipalID: principal.ID, ClientName: attachment.ClientName, Type: "remote_session.created", Summary: session.Label, Metadata: map[string]any{"attachment_id": attachment.ID}, CreatedAt: now}
	sequence, err := insertEventTx(ctx, tx, createdEvent)
	if err != nil {
		return CreateResult{}, err
	}
	createdEvent.Sequence = sequence
	result := CreateResult{Session: session, Attachment: attachment, ResumeToken: resumeToken, ExpiresAt: expiresAt}
	if in.ClientRequestID != "" {
		// Idempotency records deliberately exclude the one-time resume token.
		// A retry returns the original session and tells the caller that the
		// credential was already issued in the first response.
		cachedResult := result
		cachedResult.ResumeToken = ""
		cachedResult.ResumeTokenAlreadyIssued = true
		encoded, _ := json.Marshal(cachedResult)
		_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_records
            (remote_session_id, principal_id, client_request_id, operation, response_json, created_at, expires_at)
            VALUES ('', ?, ?, 'remote_session_create', ?, ?, ?)`,
			principal.ID, in.ClientRequestID, string(encoded), now.UnixMilli(), now.Add(24*time.Hour).UnixMilli())
		if err != nil {
			return CreateResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CreateResult{}, err
	}
	s.notifyEvent(session, createdEvent)
	return result, nil
}

func (s *Service) List(ctx context.Context, principal auth.Principal, in ListInput) (ListResult, error) {
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 20
	}
	query := `SELECT rs.id, rs.workspace_id, rs.workspace_name, rs.workspace_path, rs.label, rs.description,
        rs.status, rs.owner_principal_id, m.role, COALESCE(rs.base_git_head,''),
        COALESCE(rs.base_tree_digest,''), COALESCE(rs.environment_snapshot_id,''),
        rs.version, rs.created_at, rs.last_active_at, rs.closed_at
        FROM remote_sessions rs JOIN remote_session_members m ON m.remote_session_id = rs.id
        WHERE m.principal_id = ?`
	args := []any{principal.ID}
	if in.WorkspaceID != "" {
		query += " AND rs.workspace_id = ?"
		args = append(args, in.WorkspaceID)
	} else if in.Workspace != "" {
		query += " AND rs.workspace_name = ?"
		args = append(args, in.Workspace)
	}
	if in.Query != "" {
		query += " AND (rs.id LIKE ? OR rs.label LIKE ? OR rs.description LIKE ?)"
		like := "%" + in.Query + "%"
		args = append(args, like, like, like)
	}
	if len(in.Statuses) > 0 {
		query += " AND rs.status IN (" + placeholders(len(in.Statuses)) + ")"
		for _, status := range in.Statuses {
			args = append(args, status)
		}
	}
	if at, id, ok := decodeCursor(in.Cursor); ok {
		query += " AND (rs.last_active_at < ? OR (rs.last_active_at = ? AND rs.id < ?))"
		args = append(args, at, at, id)
	}
	query += " ORDER BY rs.last_active_at DESC, rs.id DESC LIMIT ?"
	args = append(args, in.Limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return ListResult{}, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	result := ListResult{Sessions: sessions}
	if len(sessions) > in.Limit {
		last := sessions[in.Limit-1]
		result.Sessions = sessions[:in.Limit]
		result.NextCursor = encodeCursor(last.LastActiveAt.UnixMilli(), last.ID)
	}
	return result, nil
}

func (s *Service) Get(ctx context.Context, principal auth.Principal, sessionID string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT rs.id, rs.workspace_id, rs.workspace_name, rs.workspace_path, rs.label,
        rs.description, rs.status, rs.owner_principal_id, m.role, COALESCE(rs.base_git_head,''),
        COALESCE(rs.base_tree_digest,''), COALESCE(rs.environment_snapshot_id,''), rs.version,
        rs.created_at, rs.last_active_at, rs.closed_at
        FROM remote_sessions rs JOIN remote_session_members m ON m.remote_session_id = rs.id
        WHERE rs.id = ? AND m.principal_id = ?`, sessionID, principal.ID)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return session, err
}

func (s *Service) BindWorkspaceID(ctx context.Context, sessionID, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if strings.TrimSpace(sessionID) == "" || workspaceID == "" {
		return fmt.Errorf("%w: workspace identity required", ErrInvalidInput)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE remote_sessions SET workspace_id = ? WHERE id = ? AND workspace_id = ''`, workspaceID, sessionID)
	return err
}

func (s *Service) Update(ctx context.Context, principal auth.Principal, sessionID, label, description, status string, expectedVersion int) (Session, error) {
	if status != "" && !validStatus(status) {
		return Session{}, fmt.Errorf("%w: invalid remote session status", ErrInvalidInput)
	}
	current, err := s.Get(ctx, principal, sessionID)
	if err != nil {
		return Session{}, err
	}
	if current.Role != "owner" && current.Role != "editor" {
		return Session{}, ErrForbidden
	}
	if expectedVersion <= 0 {
		expectedVersion = current.Version
	}
	if label == "" {
		label = current.Label
	}
	if description == "" {
		description = current.Description
	}
	if status == "" {
		status = current.Status
	}
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE remote_sessions SET label = ?, description = ?, status = ?,
        version = version + 1, last_active_at = ?, closed_at = CASE WHEN ? IN ('closed','archived') THEN ? ELSE closed_at END
        WHERE id = ? AND version = ?`, label, description, status, now.UnixMilli(), status, now.UnixMilli(), sessionID, expectedVersion)
	if err != nil {
		return Session{}, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return Session{}, ErrConflict
	}
	_ = s.AddEvent(ctx, principal, Event{RemoteSessionID: sessionID, Type: "remote_session.updated", Summary: label, CreatedAt: now})
	return s.Get(ctx, principal, sessionID)
}

func (s *Service) Handoff(ctx context.Context, principal auth.Principal, sessionID, role, note string, ttl time.Duration) (HandoffResult, error) {
	current, err := s.Get(ctx, principal, sessionID)
	if err != nil {
		return HandoffResult{}, err
	}
	if current.Role != "owner" {
		return HandoffResult{}, ErrForbidden
	}
	if role != "viewer" && role != "editor" && role != "approver" {
		return HandoffResult{}, fmt.Errorf("%w: invalid handoff role", ErrInvalidInput)
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 10 * time.Minute
	}
	token, err := randomToken("rsho_", 32)
	if err != nil {
		return HandoffResult{}, err
	}
	id, err := randomID("rsh_", 12)
	if err != nil {
		return HandoffResult{}, err
	}
	now := s.now().UTC()
	expires := now.Add(ttl)
	_, err = s.db.ExecContext(ctx, `INSERT INTO remote_session_handoffs
        (id, remote_session_id, token_hash, role, created_by, note, created_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, sessionID, tokenDigest(token), role, principal.ID, note, now.UnixMilli(), expires.UnixMilli())
	if err != nil {
		return HandoffResult{}, err
	}
	_ = s.AddEvent(ctx, principal, Event{RemoteSessionID: sessionID, Type: "remote_session.handoff_created", Summary: "handoff role " + role, CreatedAt: now})
	return HandoffResult{HandoffToken: token, ExpiresAt: expires, Role: role}, nil
}

func (s *Service) OpenAttachment(ctx context.Context, principal auth.Principal, sessionID string, in AttachmentInput) (Attachment, error) {
	current, err := s.Get(ctx, principal, strings.TrimSpace(sessionID))
	if err != nil {
		return Attachment{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	if in.ClientRequestID != "" {
		var cached string
		err := tx.QueryRowContext(ctx, `SELECT response_json FROM idempotency_records
            WHERE remote_session_id = ? AND principal_id = ? AND client_request_id = ? AND operation = 'remote_session_attachment' AND expires_at > ?`,
			current.ID, principal.ID, in.ClientRequestID, now.UnixMilli()).Scan(&cached)
		if err == nil {
			var attachment Attachment
			if json.Unmarshal([]byte(cached), &attachment) == nil {
				return attachment, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Attachment{}, err
		}
	}
	attachment, err := createAttachmentTx(ctx, tx, current.ID, principal.ID, in.ClientName, in.ClientVersion, now)
	if err != nil {
		return Attachment{}, err
	}
	if err := recordClientTx(ctx, tx, current.ID, principal.ID, in.ClientName, in.ClientVersion, now); err != nil {
		return Attachment{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_session_members SET last_active_at = ? WHERE remote_session_id = ? AND principal_id = ?`, now.UnixMilli(), current.ID, principal.ID); err != nil {
		return Attachment{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_sessions SET last_active_at = ? WHERE id = ?`, now.UnixMilli(), current.ID); err != nil {
		return Attachment{}, err
	}
	attachedEvent := Event{RemoteSessionID: current.ID, PrincipalID: principal.ID, ClientName: attachment.ClientName, Type: "remote_session.attached", Summary: "resumed existing session", Metadata: map[string]any{"attachment_id": attachment.ID}, CreatedAt: now}
	sequence, err := insertEventTx(ctx, tx, attachedEvent)
	if err != nil {
		return Attachment{}, err
	}
	attachedEvent.Sequence = sequence
	if in.ClientRequestID != "" {
		encoded, _ := json.Marshal(attachment)
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records
            (remote_session_id, principal_id, client_request_id, operation, response_json, created_at, expires_at)
            VALUES (?, ?, ?, 'remote_session_attachment', ?, ?, ?)`,
			current.ID, principal.ID, in.ClientRequestID, string(encoded), now.UnixMilli(), now.Add(24*time.Hour).UnixMilli()); err != nil {
			return Attachment{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Attachment{}, err
	}
	s.notifyEvent(current, attachedEvent)
	return attachment, nil
}

func (s *Service) Attach(ctx context.Context, principal auth.Principal, token, clientName, clientVersion string) (AttachResult, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AttachResult{}, err
	}
	defer tx.Rollback()
	if err := upsertPrincipal(ctx, tx, principal, now); err != nil {
		return AttachResult{}, err
	}
	var sessionID, role string
	err = tx.QueryRowContext(ctx, `SELECT remote_session_id, role FROM remote_session_handoffs
        WHERE token_hash = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		tokenDigest(token), now.UnixMilli()).Scan(&sessionID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachResult{}, ErrInvalidToken
	}
	if err != nil {
		return AttachResult{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE remote_session_handoffs SET consumed_at = ?, consumed_by = ?
        WHERE token_hash = ? AND consumed_at IS NULL`, now.UnixMilli(), principal.ID, tokenDigest(token))
	if err != nil {
		return AttachResult{}, err
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		return AttachResult{}, ErrInvalidToken
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_session_members
        (remote_session_id, principal_id, role, joined_at, last_active_at) VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(remote_session_id, principal_id) DO UPDATE SET
          role = CASE
            WHEN remote_session_members.role = 'owner' THEN 'owner'
            WHEN excluded.role = 'approver' THEN 'approver'
            WHEN excluded.role = 'editor' AND remote_session_members.role = 'viewer' THEN 'editor'
            ELSE remote_session_members.role END,
          last_active_at = excluded.last_active_at`,
		sessionID, principal.ID, role, now.UnixMilli(), now.UnixMilli()); err != nil {
		return AttachResult{}, err
	}
	if err := recordClientTx(ctx, tx, sessionID, principal.ID, clientName, clientVersion, now); err != nil {
		return AttachResult{}, err
	}
	attachment, err := createAttachmentTx(ctx, tx, sessionID, principal.ID, clientName, clientVersion, now)
	if err != nil {
		return AttachResult{}, err
	}
	attachedEvent := Event{RemoteSessionID: sessionID, PrincipalID: principal.ID, ClientName: attachment.ClientName, Type: "remote_session.attached", Summary: "attached as " + role, Metadata: map[string]any{"attachment_id": attachment.ID}, CreatedAt: now}
	sequence, err := insertEventTx(ctx, tx, attachedEvent)
	if err != nil {
		return AttachResult{}, err
	}
	attachedEvent.Sequence = sequence
	if err := tx.Commit(); err != nil {
		return AttachResult{}, err
	}
	session, err := s.Get(ctx, principal, sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	s.notifyEvent(session, attachedEvent)
	return AttachResult{Session: session, Attachment: attachment}, nil
}

func (s *Service) Close(ctx context.Context, principal auth.Principal, sessionID, status string) (Session, error) {
	current, err := s.Get(ctx, principal, sessionID)
	if err != nil {
		return Session{}, err
	}
	if current.Role != "owner" {
		return Session{}, ErrForbidden
	}
	if status == "" {
		status = "closed"
	}
	if status != "closed" && status != "archived" {
		return Session{}, fmt.Errorf("%w: close status must be closed or archived", ErrInvalidInput)
	}
	return s.Update(ctx, principal, sessionID, current.Label, current.Description, status, current.Version)
}

func (s *Service) AddEvent(ctx context.Context, principal auth.Principal, event Event) error {
	session, err := s.Get(ctx, principal, event.RemoteSessionID)
	if err != nil {
		return err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now().UTC()
	}
	event.PrincipalID = principal.ID
	sequence, err := insertEvent(ctx, s.db, event)
	if err != nil {
		return err
	}
	event.Sequence = sequence
	s.notifyEvent(session, event)
	return nil
}

func (s *Service) Events(ctx context.Context, principal auth.Principal, sessionID string, in EventsInput) ([]Event, error) {
	if _, err := s.Get(ctx, principal, sessionID); err != nil {
		return nil, err
	}
	if in.Limit <= 0 || in.Limit > 200 {
		in.Limit = 50
	}
	query := `SELECT sequence, remote_session_id, COALESCE(principal_id,''), COALESCE(client_name,''),
        event_type, COALESCE(operation_id,''), summary, COALESCE(resource_uri,''), metadata_json, created_at
        FROM remote_session_events WHERE remote_session_id = ? AND sequence > ?`
	args := []any{sessionID, in.AfterSequence}
	if len(in.Types) > 0 {
		query += " AND event_type IN (" + placeholders(len(in.Types)) + ")"
		for _, eventType := range in.Types {
			args = append(args, eventType)
		}
	}
	query += " ORDER BY sequence ASC LIMIT ?"
	args = append(args, in.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var metadata string
		var createdAt int64
		if err := rows.Scan(&event.Sequence, &event.RemoteSessionID, &event.PrincipalID, &event.ClientName,
			&event.Type, &event.OperationID, &event.Summary, &event.ResourceURI, &metadata, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		_ = json.Unmarshal([]byte(metadata), &event.Metadata)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Service) SetEnvironmentSnapshot(ctx context.Context, principal auth.Principal, sessionID, snapshotID string) error {
	if _, err := s.Get(ctx, principal, sessionID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE remote_sessions SET environment_snapshot_id = ?, version = version + 1,
        last_active_at = ? WHERE id = ?`, snapshotID, s.now().UTC().UnixMilli(), sessionID)
	return err
}

func upsertPrincipal(ctx context.Context, tx *sql.Tx, principal auth.Principal, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO principals(id, kind, subject_hash, created_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		principal.ID, principal.Kind, principal.SubjectHash, now.UnixMilli(), now.UnixMilli())
	return err
}

func createAttachmentTx(ctx context.Context, tx *sql.Tx, sessionID, principalID, clientName, clientVersion string, now time.Time) (Attachment, error) {
	clientName = normalizeClientName(clientName)
	id, err := newAttachmentID(now)
	if err != nil {
		return Attachment{}, err
	}
	attachment := Attachment{ID: id, RemoteSessionID: sessionID, ClientName: clientName, ClientVersion: clientVersion, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_session_attachments
        (id, remote_session_id, principal_id, client_name, client_version, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`, attachment.ID, sessionID, principalID, clientName, clientVersion, now.UnixMilli()); err != nil {
		return Attachment{}, err
	}
	return attachment, nil
}

func newAttachmentID(now time.Time) (string, error) {
	suffix, err := randomID("", 8)
	if err != nil {
		return "", err
	}
	now = now.UTC()
	return fmt.Sprintf("att_%s%09dZ_%s", now.Format("20060102T150405"), now.Nanosecond(), suffix), nil
}

func normalizeClientName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}

func recordClientTx(ctx context.Context, tx *sql.Tx, sessionID, principalID, name, version string, now time.Time) error {
	name = normalizeClientName(name)
	_, err := tx.ExecContext(ctx, `INSERT INTO remote_session_clients
        (remote_session_id, principal_id, client_name, client_version, first_seen_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(remote_session_id, principal_id, client_name, client_version)
        DO UPDATE SET last_seen_at = excluded.last_seen_at`, sessionID, principalID, name, version, now.UnixMilli(), now.UnixMilli())
	return err
}

type scanner interface{ Scan(...any) error }

func scanSession(row scanner) (Session, error) {
	var session Session
	var createdAt, lastActiveAt int64
	var closedAt sql.NullInt64
	err := row.Scan(&session.ID, &session.WorkspaceID, &session.WorkspaceName, &session.WorkspacePath, &session.Label,
		&session.Description, &session.Status, &session.OwnerPrincipalID, &session.Role,
		&session.BaseGitHead, &session.BaseTreeDigest, &session.EnvironmentSnapshotID,
		&session.Version, &createdAt, &lastActiveAt, &closedAt)
	if err != nil {
		return Session{}, err
	}
	session.CreatedAt = time.UnixMilli(createdAt).UTC()
	session.LastActiveAt = time.UnixMilli(lastActiveAt).UTC()
	if closedAt.Valid {
		value := time.UnixMilli(closedAt.Int64).UTC()
		session.ClosedAt = &value
	}
	return session, nil
}

func insertEvent(ctx context.Context, db *sql.DB, event Event) (int64, error) {
	metadata, _ := json.Marshal(event.Metadata)
	result, err := db.ExecContext(ctx, `INSERT INTO remote_session_events
        (remote_session_id, principal_id, client_name, event_type, operation_id, summary, resource_uri, metadata_json, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.RemoteSessionID, nullable(event.PrincipalID), nullable(event.ClientName),
		event.Type, nullable(event.OperationID), event.Summary, nullable(event.ResourceURI), string(metadata), event.CreatedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	sequence, err := result.LastInsertId()
	return sequence, err
}

func insertEventTx(ctx context.Context, tx *sql.Tx, event Event) (int64, error) {
	metadata, _ := json.Marshal(event.Metadata)
	result, err := tx.ExecContext(ctx, `INSERT INTO remote_session_events
        (remote_session_id, principal_id, client_name, event_type, operation_id, summary, resource_uri, metadata_json, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.RemoteSessionID, nullable(event.PrincipalID), nullable(event.ClientName),
		event.Type, nullable(event.OperationID), event.Summary, nullable(event.ResourceURI), string(metadata), event.CreatedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	sequence, err := result.LastInsertId()
	return sequence, err
}

func validStatus(status string) bool {
	switch status {
	case "active", "idle", "blocked", "closed", "archived":
		return true
	default:
		return false
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomID(prefix string, size int) (string, error) {
	var b = make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func randomToken(prefix string, size int) (string, error) {
	return randomID(prefix, size)
}

func encodeCursor(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + "|" + id))
}

func decodeCursor(cursor string) (int64, string, bool) {
	if cursor == "" {
		return 0, "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", false
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	return at, parts[1], err == nil
}
