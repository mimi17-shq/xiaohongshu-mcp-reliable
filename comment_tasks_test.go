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
)

func testCommentTaskRequest() *CommentTaskRequest {
	return &CommentTaskRequest{FeedID: "feed-1", XsecToken: "token-1", Content: "comment-1"}
}

func waitCommentTaskStatus(t *testing.T, manager *CommentTaskManager, id string, want CommentTaskStatus) CommentTaskView {
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
	t.Fatalf("comment task %s status=%s, want %s", id, view.Status, want)
	return CommentTaskView{}
}

func TestCommentTaskReturnsImmediatelyAndDeduplicates(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	manager, err := NewCommentTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, string, string, string) (*PostCommentResponse, error) {
		calls.Add(1)
		<-release
		return &PostCommentResponse{FeedID: "feed-1", Success: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Submit(testCommentTaskRequest(), "comment-key")
	if err != nil || first.Status != CommentTaskQueued {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	duplicate, err := manager.Submit(testCommentTaskRequest(), "comment-key")
	if err != nil || duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	changed := testCommentTaskRequest()
	changed.Content = "different"
	if _, err := manager.Submit(changed, "comment-key"); err == nil {
		t.Fatal("same key with changed request must fail")
	}
	close(release)
	finished := waitCommentTaskStatus(t, manager, first.TaskID, CommentTaskConfirmed)
	if finished.Result == nil || !finished.Result.Success || calls.Load() != 1 {
		t.Fatalf("finished=%+v calls=%d", finished, calls.Load())
	}
}

func TestCommentTaskUnknownIsDurableAndRequestIsScrubbed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	manager, _ := NewCommentTaskManager(path, func(context.Context, string, string, string) (*PostCommentResponse, error) {
		return nil, errors.New("client timed out")
	})
	manager.Start()
	view, err := manager.Submit(testCommentTaskRequest(), "unknown-key")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitCommentTaskStatus(t, manager, view.TaskID, CommentTaskUnknown)
	if !strings.Contains(finished.Error, "timed out") {
		t.Fatalf("finished=%+v", finished)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token-1") || strings.Contains(string(data), "comment-1") {
		t.Fatalf("terminal task leaked request: %s", data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("task file mode info=%v err=%v", info, err)
	}
}

func TestCommentTaskRestartMarksRunningUnknownAndResumesQueued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Now().UTC()
	stored := commentTaskFile{Version: 1, Tasks: map[string]*CommentTask{
		"running": {ID: "running", Status: CommentTaskRunning, FeedID: "feed-1", Request: testCommentTaskRequest(), CreatedAt: now, UpdatedAt: now},
		"queued":  {ID: "queued", Status: CommentTaskQueued, FeedID: "feed-2", Request: &CommentTaskRequest{FeedID: "feed-2", XsecToken: "token", Content: "queued"}, CreatedAt: now, UpdatedAt: now},
	}}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewCommentTaskManager(path, func(_ context.Context, feedID, _, _ string) (*PostCommentResponse, error) {
		return &PostCommentResponse{FeedID: feedID, Success: true}, nil
	})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	waitCommentTaskStatus(t, manager, "running", CommentTaskUnknown)
	waitCommentTaskStatus(t, manager, "queued", CommentTaskConfirmed)
}

func TestCommentTaskFindByFeedNewestFirstWithoutPrivateRequest(t *testing.T) {
	manager, _ := NewCommentTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, feedID, _, _ string) (*PostCommentResponse, error) {
		return &PostCommentResponse{FeedID: feedID, Success: true}, nil
	})
	manager.Start()
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	first, _ := manager.Submit(testCommentTaskRequest(), "one")
	waitCommentTaskStatus(t, manager, first.TaskID, CommentTaskConfirmed)
	now = now.Add(time.Second)
	secondReq := testCommentTaskRequest()
	secondReq.Content = "comment-2"
	second, _ := manager.Submit(secondReq, "two")
	waitCommentTaskStatus(t, manager, second.TaskID, CommentTaskConfirmed)
	lookup := manager.FindByFeed("feed-1")
	if lookup.MatchCount != 2 || lookup.Tasks[0].TaskID != second.TaskID {
		t.Fatalf("lookup=%+v", lookup)
	}
	encoded, _ := json.Marshal(lookup)
	if containsAny(string(encoded), "token-1", "comment-1", "comment-2") {
		t.Fatalf("lookup leaked request data: %s", encoded)
	}
}

func TestCommentTaskFallbackHashExpiresAfterTenMinutes(t *testing.T) {
	manager, _ := NewCommentTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, feedID, _, _ string) (*PostCommentResponse, error) {
		return &PostCommentResponse{FeedID: feedID, Success: true}, nil
	})
	manager.Start()
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	first, _ := manager.Submit(testCommentTaskRequest(), "")
	duplicate, err := manager.Submit(testCommentTaskRequest(), "")
	if err != nil || duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	waitCommentTaskStatus(t, manager, first.TaskID, CommentTaskConfirmed)
	now = now.Add(commentFallbackIdempotencyWindow + time.Second)
	after, err := manager.Submit(testCommentTaskRequest(), "")
	if err != nil || after.TaskID == first.TaskID {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	waitCommentTaskStatus(t, manager, after.TaskID, CommentTaskConfirmed)
}

func TestGetPublishStatusCompatibilityAcceptsCommentTaskID(t *testing.T) {
	manager, _ := NewCommentTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, feedID, _, _ string) (*PostCommentResponse, error) {
		return &PostCommentResponse{FeedID: feedID, Success: true}, nil
	})
	view, err := manager.Submit(testCommentTaskRequest(), "compat-key")
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppServer(NewXiaohongshuService(), "")
	app.commentTasks = manager
	result := app.handleGetPublishStatus(context.Background(), view.TaskID)
	if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, view.TaskID) {
		t.Fatalf("compatibility result=%+v", result)
	}
}
