package remotesession

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mcpx/internal/auth"
	"mcpx/internal/state"
)

func testService(t *testing.T) (*Service, *state.Store) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store.DB()), store
}

func testPrincipal(id string) auth.Principal {
	return auth.Principal{ID: id, Kind: "test", SubjectHash: id + "-hash"}
}

func TestCreateListGetAndIdempotency(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	in := CreateInput{
		WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "session",
		ClientRequestID: "create-1", ClientName: "client-a", ClientVersion: "1",
	}
	first, err := service.Create(context.Background(), owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(first.Session.ID); err != nil {
		t.Fatalf("remote session id is not UUID: %q (%v)", first.Session.ID, err)
	}
	second, err := service.Create(context.Background(), owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.ID != second.Session.ID {
		t.Fatalf("idempotent result changed: %+v %+v", first, second)
	}
	if first.Attachment.ID == "" || first.Attachment.RemoteSessionID != first.Session.ID || !strings.HasPrefix(first.Attachment.ID, "att_") {
		t.Fatalf("initial attachment=%+v", first.Attachment)
	}
	if first.Attachment.ID != second.Attachment.ID {
		t.Fatalf("idempotent create changed attachment: first=%+v second=%+v", first.Attachment, second.Attachment)
	}
	if first.ResumeToken == "" || second.ResumeToken != "" || !second.ResumeTokenAlreadyIssued {
		t.Fatalf("one-time token contract violated: first=%+v second=%+v", first, second)
	}
	var cached string
	if err := service.db.QueryRow(`SELECT response_json FROM idempotency_records
		WHERE principal_id = ? AND client_request_id = ?`, owner.ID, in.ClientRequestID).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cached, first.ResumeToken) {
		t.Fatal("resume token was persisted in idempotency record")
	}
	list, err := service.List(context.Background(), owner, ListInput{Workspace: "mcpx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != first.Session.ID {
		t.Fatalf("list: %+v", list)
	}
	got, err := service.Get(context.Background(), owner, first.Session.ID)
	if err != nil || got.Role != "owner" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
}

func TestHandoffAttachIsOneShotAndACLFiltered(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	other := testPrincipal("other")
	created, err := service.Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "handoff"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), other, created.Session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized get: %v", err)
	}
	handoff, err := service.Handoff(context.Background(), owner, created.Session.ID, "editor", "continue", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := service.Attach(context.Background(), other, handoff.HandoffToken, "client-b", "2")
	if err != nil {
		t.Fatal(err)
	}
	if attached.Session.ID != created.Session.ID || attached.Session.Role != "editor" || attached.Attachment.RemoteSessionID != created.Session.ID {
		t.Fatalf("attached: %+v", attached)
	}
	if _, err := service.Attach(context.Background(), testPrincipal("third"), handoff.HandoffToken, "client-c", "1"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token should be consumed: %v", err)
	}
	list, err := service.List(context.Background(), other, ListInput{})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatalf("attached list: %+v err=%v", list, err)
	}
}

func TestOpenAttachmentCreatesNewContextAndRetriesIdempotently(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	created, err := service.Create(context.Background(), owner, CreateInput{
		WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), ClientRequestID: "open-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.OpenAttachment(context.Background(), owner, created.Session.ID, AttachmentInput{ClientRequestID: "resume-b", ClientName: "client-b", ClientVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == created.Attachment.ID {
		t.Fatalf("new model context reused prior attachment: %q", second.ID)
	}
	retry, err := service.OpenAttachment(context.Background(), owner, created.Session.ID, AttachmentInput{ClientRequestID: "resume-b", ClientName: "client-b", ClientVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != second.ID {
		t.Fatalf("idempotent resume changed attachment: %q != %q", retry.ID, second.ID)
	}
	third, err := service.OpenAttachment(context.Background(), owner, created.Session.ID, AttachmentInput{ClientRequestID: "resume-c", ClientName: "client-b", ClientVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == second.ID {
		t.Fatalf("distinct resume request reused attachment: %q", third.ID)
	}
	var count int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM remote_session_attachments WHERE remote_session_id = ?`, created.Session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("attachment rows=%d want=3", count)
	}
}

func TestAttachmentIDCarriesSortableUTCTime(t *testing.T) {
	first, err := newAttachmentID(time.Date(2026, 8, 18, 5, 9, 12, 123456789, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := newAttachmentID(time.Date(2026, 8, 18, 5, 9, 13, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "att_20260818T050912123456789Z_") {
		t.Fatalf("attachment ID does not expose UTC timestamp: %q", first)
	}
	if first >= second {
		t.Fatalf("attachment IDs are not time ordered: %q >= %q", first, second)
	}
}

func TestUpdateUsesOptimisticVersion(t *testing.T) {
	service, _ := testService(t)
	owner := testPrincipal("owner")
	created, err := service.Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: t.TempDir(), Label: "before"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), owner, created.Session.ID, "after", "", "idle", created.Session.Version)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != "after" || updated.Status != "idle" || updated.Version != 2 {
		t.Fatalf("updated: %+v", updated)
	}
	if _, err := service.Update(context.Background(), owner, created.Session.ID, "stale", "", "", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestSessionSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcpx.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := testPrincipal("owner")
	created, err := NewService(store.DB()).Create(context.Background(), owner, CreateInput{WorkspaceName: "mcpx", WorkspacePath: dir, Label: "persist"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := NewService(reopened.DB()).Get(context.Background(), owner, created.Session.ID)
	if err != nil || got.Label != "persist" {
		t.Fatalf("reopened: %+v err=%v", got, err)
	}
}
