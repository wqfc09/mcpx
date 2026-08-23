// Package plan persists and advances MCPX execution plans independently from
// terminal Execution Tasks.
package plan

import (
	"errors"
	"strings"
	"time"
)

const (
	EvidenceRead         = "read"
	EvidenceEdit         = "edit"
	EvidenceExecute      = "execute"
	EvidenceArtifact     = "artifact"
	EvidenceSource       = "source"
	EvidenceVerification = "verification"
	EvidenceObserve      = "observe"

	PlanReady      = "ready"
	PlanInProgress = "in_progress"
	PlanBlocked    = "blocked"
	PlanCompleted  = "completed"
	PlanCancelled  = "cancelled"

	TaskTodo       = "todo"
	TaskInProgress = "in_progress"
	TaskBlocked    = "blocked"
	TaskCompleted  = "completed"
	TaskSkipped    = "skipped"
)

var (
	ErrNotFound         = errors.New("plan not found")
	ErrInvalidInput     = errors.New("invalid plan input")
	ErrInvalidState     = errors.New("invalid plan state transition")
	ErrDependency       = errors.New("plan dependency is not satisfied")
	ErrCycle            = errors.New("plan task dependency cycle")
	ErrEvidence         = errors.New("invalid plan evidence")
	ErrEvidenceRequired = errors.New("completed plan task requires evidence")
)

type Plan struct {
	ID              string     `json:"plan_id"`
	RemoteSessionID string     `json:"remote_session_id"`
	Goal            string     `json:"goal"`
	Summary         string     `json:"summary,omitempty"`
	Status          string     `json:"status"`
	Version         int        `json:"version"`
	CreatedBy       string     `json:"created_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	Tasks           []Task     `json:"tasks"`
	Progress        Progress   `json:"progress"`
	Events          []Event    `json:"events,omitempty"`
}

type Task struct {
	ID          string     `json:"plan_task_id"`
	PlanID      string     `json:"plan_id"`
	Ordinal     int        `json:"ordinal"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	DependsOn   []string   `json:"depends_on,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Evidence    []Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	ID            string         `json:"evidence_id"`
	PlanID        string         `json:"plan_id"`
	TaskID        string         `json:"plan_task_id"`
	Kind          string         `json:"kind"`
	ReferenceID   string         `json:"reference_id"`
	Validated     bool           `json:"validated"`
	SourceEventID string         `json:"source_event_id,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	CreatedBy     string         `json:"created_by,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type Event struct {
	ID        string         `json:"event_id"`
	PlanID    string         `json:"plan_id"`
	TaskID    string         `json:"plan_task_id,omitempty"`
	Type      string         `json:"type"`
	Reason    string         `json:"reason,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedBy string         `json:"created_by,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type Progress struct {
	Total      int `json:"total"`
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Completed  int `json:"completed"`
	Skipped    int `json:"skipped"`
}

type TaskInput struct {
	ID          string   `json:"local_id,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

type CreateInput struct {
	Goal    string      `json:"goal"`
	Summary string      `json:"summary,omitempty"`
	Tasks   []TaskInput `json:"tasks"`
}

type EvidenceInput struct {
	Kind          string         `json:"kind"`
	ReferenceID   string         `json:"reference_id"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	WorkspaceRoot string         `json:"-"`
}

var canonicalEvidenceKinds = []string{
	EvidenceRead, EvidenceEdit, EvidenceExecute, EvidenceArtifact, EvidenceSource, EvidenceVerification, EvidenceObserve,
}

func EvidenceKinds() []string {
	return append([]string(nil), canonicalEvidenceKinds...)
}

func IsEvidenceKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	for _, candidate := range canonicalEvidenceKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

func IsEvidenceError(err error) bool { return errors.Is(err, ErrEvidence) }

type TaskOperation struct {
	Action      string   `json:"action"`
	TaskID      string   `json:"plan_task_id,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

type ReplanInput struct {
	Goal       string          `json:"goal,omitempty"`
	Summary    string          `json:"summary,omitempty"`
	Reason     string          `json:"reason"`
	Operations []TaskOperation `json:"operations"`
}

type DeliveryCheck struct {
	Code    string         `json:"code"`
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type Delivery struct {
	PlanID          string          `json:"plan_id"`
	RemoteSessionID string          `json:"remote_session_id"`
	Status          string          `json:"status"`
	Ready           bool            `json:"ready"`
	Checks          []DeliveryCheck `json:"checks"`
	Blockers        []string        `json:"blockers,omitempty"`
	Plan            Plan            `json:"plan"`
	DeliveredAt     *time.Time      `json:"delivered_at,omitempty"`
}
