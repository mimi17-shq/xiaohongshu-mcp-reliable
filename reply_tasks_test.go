package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func testReplyTaskRequest() *ReplyTaskRequest {
	return &ReplyTaskRequest{
		FeedID: "feed-1", XsecToken: "token-1", CommentID: "comment-1", Content: "reply-1",
	}
}

func confirmedReplyResponse() *ReplyCommentResponse {
	return &ReplyCommentResponse{
		FeedID: "feed-1", TargetCommentID: "comment-1", Success: true,
		VerificationStatus:  xiaohongshu.ReplyConfirmed,
		RequestStatus:       xiaohongshu.ReplyRequestAllowed,
		ServerStatus:        xiaohongshu.ReplyServerAccepted,
		ReplyButtonAttempts: 2,
	}
}

func waitReplyTaskStatus(t *testing.T, manager *ReplyTaskManager, id string, want ReplyTaskStatus) ReplyTaskView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, ok := manager.Get(id)
		if ok && view.Status == want {
			return view
		}
		time.Sleep(5 * time.Millisecond)
	}
	view, _ := manager.Get(id)
	t.Fatalf("reply task %s status=%s, want %s", id, view.Status, want)
	return ReplyTaskView{}
}

func TestReplyTaskReturnsImmediatelyAndDeduplicates(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	manager, err := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		calls.Add(1)
		<-release
		return confirmedReplyResponse(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Submit(testReplyTaskRequest(), "reply-key")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != ReplyTaskQueued || first.RequestStatus != xiaohongshu.ReplyRequestNotStarted || first.Phase != xiaohongshu.ReplyPhaseQueued || first.PhaseUpdatedAt.IsZero() {
		t.Fatalf("initial view=%+v", first)
	}
	duplicate, err := manager.Submit(testReplyTaskRequest(), "reply-key")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("duplicate=%+v first=%+v", duplicate, first)
	}
	changed := testReplyTaskRequest()
	changed.Content = "different"
	if _, err := manager.Submit(changed, "reply-key"); err == nil {
		t.Fatal("reusing one idempotency key for different content must fail")
	}
	close(release)
	finished := waitReplyTaskStatus(t, manager, first.TaskID, ReplyTaskConfirmed)
	if finished.RequestStatus != xiaohongshu.ReplyRequestAllowed || !finished.Result.Success || finished.ReplyButtonAttempts != 2 {
		t.Fatalf("finished=%+v", finished)
	}
	afterSuccess, err := manager.Submit(testReplyTaskRequest(), "reply-key")
	if err != nil {
		t.Fatal(err)
	}
	if afterSuccess.TaskID != first.TaskID || !afterSuccess.Deduplicated || afterSuccess.Status != ReplyTaskConfirmed {
		t.Fatalf("after-success duplicate=%+v first=%+v", afterSuccess, first)
	}
	if calls.Load() != 1 {
		t.Fatalf("executor calls=%d, want 1", calls.Load())
	}
}

func TestReplyTaskPreservesUncertainTerminalStatus(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return &ReplyCommentResponse{
			FeedID: "feed-1", TargetCommentID: "comment-1",
			VerificationStatus: xiaohongshu.ReplyUncertain,
			RequestStatus:      xiaohongshu.ReplyRequestAllowed,
		}, errors.New("submitted but observation incomplete")
	})
	manager.Start()
	view, err := manager.Submit(testReplyTaskRequest(), "uncertain-key")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitReplyTaskStatus(t, manager, view.TaskID, ReplyTaskUncertain)
	if finished.RequestStatus != xiaohongshu.ReplyRequestAllowed || finished.Result == nil {
		t.Fatalf("uncertain result=%+v", finished)
	}
}

func TestReplyTaskPreservesExplicitServerRejection(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return &ReplyCommentResponse{
			FeedID: "feed-1", TargetCommentID: "comment-1",
			VerificationStatus: xiaohongshu.ReplyRejected,
			RequestStatus:      xiaohongshu.ReplyRequestAllowed,
			ServerStatus:       xiaohongshu.ReplyServerRejected,
		}, errors.New("server rejected")
	})
	manager.Start()
	view, err := manager.Submit(testReplyTaskRequest(), "rejected-key")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitReplyTaskStatus(t, manager, view.TaskID, ReplyTaskRejected)
	if finished.RequestStatus != xiaohongshu.ReplyRequestAllowed || finished.ServerStatus != xiaohongshu.ReplyServerRejected {
		t.Fatalf("rejected result=%+v", finished)
	}
}

func TestReplyTaskPersistsServerAcceptanceWhileVerifying(t *testing.T) {
	progressed := make(chan struct{})
	release := make(chan struct{})
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, _, _, _, _, _ string, progress replyProgress) (*ReplyCommentResponse, error) {
		progress(xiaohongshu.ReplyUncertain, xiaohongshu.ReplyRequestAllowed, xiaohongshu.ReplyServerAccepted, xiaohongshu.ReplyPhaseSubmittedPendingVerification)
		close(progressed)
		<-release
		return confirmedReplyResponse(), nil
	})
	manager.Start()
	view, err := manager.Submit(testReplyTaskRequest(), "server-progress-key")
	if err != nil {
		t.Fatal(err)
	}
	<-progressed
	running, ok := manager.Get(view.TaskID)
	if !ok || running.Status != ReplyTaskRunning || running.ServerStatus != xiaohongshu.ReplyServerAccepted {
		t.Fatalf("running server progress=%+v, found=%v", running, ok)
	}
	close(release)
	waitReplyTaskStatus(t, manager, view.TaskID, ReplyTaskConfirmed)
}

func TestReplyTaskPersistsAllowedRequestWhileStillRunning(t *testing.T) {
	progressed := make(chan struct{})
	release := make(chan struct{})
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, _, _, _, _, _ string, progress replyProgress) (*ReplyCommentResponse, error) {
		progress(xiaohongshu.ReplyUncertain, xiaohongshu.ReplyRequestAllowed, xiaohongshu.ReplyServerNotObserved, xiaohongshu.ReplyPhaseSubmittedPendingVerification)
		close(progressed)
		<-release
		return confirmedReplyResponse(), nil
	})
	manager.Start()
	view, err := manager.Submit(testReplyTaskRequest(), "progress-key")
	if err != nil {
		t.Fatal(err)
	}
	<-progressed
	running, ok := manager.Get(view.TaskID)
	if !ok || running.Status != ReplyTaskRunning || running.RequestStatus != xiaohongshu.ReplyRequestAllowed || running.Phase != xiaohongshu.ReplyPhaseSubmittedPendingVerification {
		t.Fatalf("running progress=%+v, found=%v", running, ok)
	}
	close(release)
	waitReplyTaskStatus(t, manager, view.TaskID, ReplyTaskConfirmed)
}

func TestReplyTaskPersistsCoarseProgressPhasesWithoutHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	reached := make(chan xiaohongshu.ReplyProgressPhase)
	advance := make(chan struct{})
	phases := []xiaohongshu.ReplyProgressPhase{
		xiaohongshu.ReplyPhaseLocatingComment,
		xiaohongshu.ReplyPhaseTargetLocated,
		xiaohongshu.ReplyPhaseReplyEditorOpen,
		xiaohongshu.ReplyPhaseReplyContentEntered,
		xiaohongshu.ReplyPhaseSubmittedPendingVerification,
	}
	manager, _ := NewReplyTaskManager(path, func(_ context.Context, _, _, _, _, _ string, progress replyProgress) (*ReplyCommentResponse, error) {
		for _, phase := range phases {
			requestStatus := xiaohongshu.ReplyRequestNotStarted
			if phase == xiaohongshu.ReplyPhaseSubmittedPendingVerification {
				requestStatus = xiaohongshu.ReplyRequestAllowed
			}
			progress(xiaohongshu.ReplyUncertain, requestStatus, xiaohongshu.ReplyServerNotObserved, phase)
			reached <- phase
			<-advance
		}
		return confirmedReplyResponse(), nil
	})
	manager.Start()
	view, err := manager.Submit(testReplyTaskRequest(), "phase-key")
	if err != nil {
		t.Fatal(err)
	}
	previousPhaseTime := view.PhaseUpdatedAt
	previousUpdatedAt := view.UpdatedAt
	for _, expected := range phases {
		if got := <-reached; got != expected {
			t.Fatalf("reported phase=%s, want %s", got, expected)
		}
		current, ok := manager.Get(view.TaskID)
		if !ok || current.Phase != expected {
			t.Fatalf("current phase=%+v, found=%v, want %s", current, ok, expected)
		}
		if !current.PhaseUpdatedAt.After(previousPhaseTime) || !current.UpdatedAt.After(previousUpdatedAt) {
			t.Fatalf("timestamps did not advance: previous phase=%s updated=%s current=%+v", previousPhaseTime, previousUpdatedAt, current)
		}
		previousPhaseTime, previousUpdatedAt = current.PhaseUpdatedAt, current.UpdatedAt
		data, readErr := os.ReadFile(path)
		if readErr != nil || !strings.Contains(string(data), `"phase": "`+string(expected)+`"`) {
			t.Fatalf("phase was not persisted: err=%v data=%s", readErr, data)
		}
		advance <- struct{}{}
	}
	finished := waitReplyTaskStatus(t, manager, view.TaskID, ReplyTaskConfirmed)
	if finished.Phase != xiaohongshu.ReplyPhaseSubmittedPendingVerification || !finished.PhaseUpdatedAt.Equal(previousPhaseTime) {
		t.Fatalf("terminal task lost last real phase: %+v", finished)
	}
}

func TestReplyTaskIgnoresDuplicateAndBackwardPhaseEvents(t *testing.T) {
	now := time.Now().UTC()
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	manager.tasks["phase"] = &ReplyTask{
		ID: "phase", Status: ReplyTaskRunning, RequestStatus: xiaohongshu.ReplyRequestNotStarted,
		ServerStatus: xiaohongshu.ReplyServerNotObserved,
		Phase:        xiaohongshu.ReplyPhaseTargetLocated, PhaseUpdatedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	manager.recordProgress("phase", xiaohongshu.ReplyUncertain, xiaohongshu.ReplyRequestNotStarted, xiaohongshu.ReplyServerNotObserved, xiaohongshu.ReplyPhaseTargetLocated)
	manager.recordProgress("phase", xiaohongshu.ReplyUncertain, xiaohongshu.ReplyRequestNotStarted, xiaohongshu.ReplyServerNotObserved, xiaohongshu.ReplyPhaseLocatingComment)
	view, _ := manager.Get("phase")
	if view.Phase != xiaohongshu.ReplyPhaseTargetLocated || !view.PhaseUpdatedAt.Equal(now) || !view.UpdatedAt.Equal(now) {
		t.Fatalf("duplicate/backward phase changed task: %+v", view)
	}
}

func TestReplyTaskCanBeFoundByFeedAndCommentNewestFirst(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	manager.Start()
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	first, _ := manager.Submit(testReplyTaskRequest(), "one")
	waitReplyTaskStatus(t, manager, first.TaskID, ReplyTaskConfirmed)
	now = now.Add(replyTargetCooldownWindow + time.Second)
	secondReq := testReplyTaskRequest()
	secondReq.Content = "reply-2"
	second, _ := manager.Submit(secondReq, "two")
	waitReplyTaskStatus(t, manager, second.TaskID, ReplyTaskConfirmed)

	lookup := manager.FindByTarget("feed-1", "comment-1")
	if lookup.MatchCount != 2 || lookup.Tasks[0].TaskID != second.TaskID || lookup.Tasks[1].TaskID != first.TaskID {
		t.Fatalf("lookup=%+v", lookup)
	}
	data, _ := json.Marshal(lookup)
	if string(data) == "" || containsAny(string(data), "token-1", "reply-1", "reply-2") {
		t.Fatalf("lookup leaked request data: %s", data)
	}
}

func TestReplyTaskTargetCooldownBlocksChangedKeyAndContent(t *testing.T) {
	release := make(chan struct{})
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		<-release
		return confirmedReplyResponse(), nil
	})
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	manager.Start()
	first, err := manager.Submit(testReplyTaskRequest(), "first-key")
	if err != nil {
		t.Fatal(err)
	}
	waitReplyTaskStatus(t, manager, first.TaskID, ReplyTaskRunning)
	now = now.Add(time.Hour)
	changed := testReplyTaskRequest()
	changed.Content = "rewritten reply"
	_, err = manager.Submit(changed, "different-key")
	if err == nil || !strings.Contains(err.Error(), first.TaskID) || !strings.Contains(err.Error(), "未创建新任务") {
		t.Fatalf("target cooldown error=%v", err)
	}
	close(release)
	waitReplyTaskStatus(t, manager, first.TaskID, ReplyTaskConfirmed)
}

func TestReplyTaskTargetCooldownCoversRecentConfirmedAndUncertainOnly(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	for _, status := range []ReplyTaskStatus{ReplyTaskConfirmed, ReplyTaskUncertain} {
		manager.tasks = map[string]*ReplyTask{
			"existing": {ID: "existing", Status: status, FeedID: "feed-1", CommentID: "comment-1", CreatedAt: now.Add(-time.Minute), UpdatedAt: now},
		}
		changed := testReplyTaskRequest()
		changed.Content = "new intent"
		if _, err := manager.Submit(changed, "new-key"); err == nil {
			t.Fatalf("status %s must trigger cooldown", status)
		}
	}
	for _, status := range []ReplyTaskStatus{ReplyTaskBlocked, ReplyTaskWrongTarget, ReplyTaskRejected} {
		manager.tasks = map[string]*ReplyTask{
			"existing": {ID: "existing", Status: status, FeedID: "feed-1", CommentID: "comment-1", CreatedAt: now.Add(-time.Minute), UpdatedAt: now},
		}
		changed := testReplyTaskRequest()
		changed.Content = "new intent " + string(status)
		if _, err := manager.Submit(changed, "key-"+string(status)); err != nil {
			t.Fatalf("status %s must not trigger cooldown: %v", status, err)
		}
	}
}

func TestReplyTaskTargetCooldownExpiresForTerminalStatus(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	manager.tasks["old"] = &ReplyTask{ID: "old", Status: ReplyTaskConfirmed, FeedID: "feed-1", CommentID: "comment-1", CreatedAt: now.Add(-replyTargetCooldownWindow - time.Second), UpdatedAt: now}
	changed := testReplyTaskRequest()
	changed.Content = "later reply"
	if _, err := manager.Submit(changed, "later-key"); err != nil {
		t.Fatalf("expired terminal cooldown blocked task: %v", err)
	}
}

func TestGetPublishStatusCompatibilityAcceptsReplyTaskID(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	view, err := manager.Submit(testReplyTaskRequest(), "compat-key")
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppServer(NewXiaohongshuService(), "")
	app.replyTasks = manager
	result := app.handleGetPublishStatus(context.Background(), view.TaskID)
	if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, view.TaskID) {
		t.Fatalf("compatibility result=%+v", result)
	}
}

func TestReplyTaskFallbackHashExpiresAfterTenMinutes(t *testing.T) {
	manager, _ := NewReplyTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return confirmedReplyResponse(), nil
	})
	manager.Start()
	now := time.Date(2026, 9, 10, 3, 20, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	first, err := manager.Submit(testReplyTaskRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := manager.Submit(testReplyTaskRequest(), "")
	if err != nil || duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("duplicate=%+v err=%v first=%+v", duplicate, err, first)
	}
	waitReplyTaskStatus(t, manager, first.TaskID, ReplyTaskConfirmed)
	now = now.Add(10*time.Minute + time.Second)
	afterWindow, err := manager.Submit(testReplyTaskRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	if afterWindow.TaskID == first.TaskID || afterWindow.Deduplicated {
		t.Fatalf("task after fallback window=%+v", afterWindow)
	}
	waitReplyTaskStatus(t, manager, afterWindow.TaskID, ReplyTaskConfirmed)
}

func TestReplyTaskRestartPreservesKnownAllowedPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Now().UTC()
	stored := replyTaskFile{Version: 1, Tasks: map[string]*ReplyTask{
		"allowed": {ID: "allowed", Status: ReplyTaskRunning, RequestStatus: xiaohongshu.ReplyRequestAllowed, FeedID: "feed-1", CommentID: "comment-1", Phase: xiaohongshu.ReplyPhaseSubmittedPendingVerification, PhaseUpdatedAt: now, Request: testReplyTaskRequest(), CreatedAt: now, UpdatedAt: now},
	}}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewReplyTaskManager(path, func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return nil, errors.New("must not resume a running task")
	})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	view := waitReplyTaskStatus(t, manager, "allowed", ReplyTaskUncertain)
	if view.RequestStatus != xiaohongshu.ReplyRequestAllowed || view.Phase != xiaohongshu.ReplyPhaseSubmittedPendingVerification || !view.PhaseUpdatedAt.Equal(now) {
		t.Fatalf("restarted allowed task=%+v", view)
	}
}

func TestReplyTaskRestartPreservesPersistedServerRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Now().UTC()
	stored := replyTaskFile{Version: 1, Tasks: map[string]*ReplyTask{
		"rejected": {
			ID: "rejected", Status: ReplyTaskRunning, RequestStatus: xiaohongshu.ReplyRequestAllowed,
			ServerStatus: xiaohongshu.ReplyServerRejected, FeedID: "feed-1", CommentID: "comment-1",
			Phase: xiaohongshu.ReplyPhaseSubmittedPendingVerification, PhaseUpdatedAt: now,
			Request: testReplyTaskRequest(), CreatedAt: now, UpdatedAt: now,
		},
	}}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewReplyTaskManager(path, func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return nil, errors.New("must not resume")
	})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	view := waitReplyTaskStatus(t, manager, "rejected", ReplyTaskRejected)
	if view.ServerStatus != xiaohongshu.ReplyServerRejected || !strings.Contains(view.Error, "明确拒绝") {
		t.Fatalf("restarted rejected task=%+v", view)
	}
}

func TestReplyTaskLoadBackfillsLegacyTerminalPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Now().UTC().Add(-time.Minute)
	stored := replyTaskFile{Version: 1, Tasks: map[string]*ReplyTask{
		"legacy": {
			ID: "legacy", Status: ReplyTaskConfirmed, RequestStatus: xiaohongshu.ReplyRequestAllowed,
			FeedID: "feed-1", CommentID: "comment-1", Result: confirmedReplyResponse(),
			CreatedAt: now.Add(-time.Minute), UpdatedAt: now, FinishedAt: &now,
		},
	}}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewReplyTaskManager(path, func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error) {
		return nil, errors.New("terminal task must not execute")
	})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	view, ok := manager.Get("legacy")
	if !ok || view.Status != ReplyTaskConfirmed || view.Phase != xiaohongshu.ReplyPhaseUnknown || !view.PhaseUpdatedAt.Equal(now) {
		t.Fatalf("legacy task migration=%+v, found=%v", view, ok)
	}
}

func TestReplyTaskRestartMarksRunningUncertainAndResumesQueued(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	now := time.Now().UTC()
	stored := replyTaskFile{Version: 1, Tasks: map[string]*ReplyTask{
		"running": {ID: "running", Status: ReplyTaskRunning, RequestStatus: xiaohongshu.ReplyRequestNotStarted, FeedID: "feed-1", CommentID: "comment-1", Request: testReplyTaskRequest(), CreatedAt: now, UpdatedAt: now},
		"queued":  {ID: "queued", Status: ReplyTaskQueued, RequestStatus: xiaohongshu.ReplyRequestNotStarted, FeedID: "feed-2", CommentID: "comment-2", Request: &ReplyTaskRequest{FeedID: "feed-2", XsecToken: "token", CommentID: "comment-2", Content: "reply"}, CreatedAt: now, UpdatedAt: now},
	}}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewReplyTaskManager(path, func(_ context.Context, feedID, _ string, commentID, _, _ string, _ replyProgress) (*ReplyCommentResponse, error) {
		return &ReplyCommentResponse{FeedID: feedID, TargetCommentID: commentID, Success: true, VerificationStatus: xiaohongshu.ReplyConfirmed, RequestStatus: xiaohongshu.ReplyRequestAllowed}, nil
	})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	running := waitReplyTaskStatus(t, manager, "running", ReplyTaskUncertain)
	if running.RequestStatus != xiaohongshu.ReplyRequestNotObserved {
		t.Fatalf("restarted running task=%+v", running)
	}
	waitReplyTaskStatus(t, manager, "queued", ReplyTaskConfirmed)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("task file mode=%v, want 600", info.Mode().Perm())
	}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}
