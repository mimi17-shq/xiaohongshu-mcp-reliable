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

func testPublishRequest() *PublishRequest {
	return &PublishRequest{
		Title:   "测试标题",
		Content: "测试正文",
		Images:  []string{"https://example.com/test-image.png"},
		Tags:    []string{"测试"},
	}
}

func TestPublishTaskRejectsUnreadableLocalImageBeforeCreatingTask(t *testing.T) {
	var called atomic.Bool
	path := filepath.Join(t.TempDir(), "tasks.json")
	manager, err := NewPublishTaskManager(path, func(context.Context, *PublishRequest) (*PublishResponse, error) {
		called.Store(true)
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()

	req := testPublishRequest()
	req.Images = []string{filepath.Join(t.TempDir(), "missing.png")}
	if _, err := manager.Submit(req, "missing-local-image"); err == nil || !strings.Contains(err.Error(), "第1张本地图片不可读") {
		t.Fatalf("Submit error=%v, want readable local image error", err)
	}
	if called.Load() {
		t.Fatal("executor must not run for an unreadable local image")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("task file should not be created, stat error=%v", err)
	}
}

func TestPublishTaskAcceptsReadableLocalImage(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(imagePath, []byte("test image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewPublishTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, *PublishRequest) (*PublishResponse, error) {
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()

	req := testPublishRequest()
	req.Images = []string{imagePath}
	view, err := manager.Submit(req, "readable-local-image")
	if err != nil {
		t.Fatal(err)
	}
	waitTaskStatus(t, manager, view.TaskID, PublishTaskSucceeded)
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	deduplicated, err := manager.Submit(req, "readable-local-image")
	if err != nil {
		t.Fatalf("existing idempotent task should survive later image removal: %v", err)
	}
	if deduplicated.TaskID != view.TaskID || !deduplicated.Deduplicated {
		t.Fatalf("deduplicated=%+v, original=%+v", deduplicated, view)
	}
}

func waitTaskStatus(t *testing.T, manager *PublishTaskManager, id string, want PublishTaskStatus) PublishTaskView {
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
	t.Fatalf("task %s status=%s, want %s", id, view.Status, want)
	return PublishTaskView{}
}

func TestPublishTaskExplicitIdempotency(t *testing.T) {
	release := make(chan struct{})
	manager, err := NewPublishTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, *PublishRequest) (*PublishResponse, error) {
		<-release
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()

	first, err := manager.Submit(testPublishRequest(), "request-123")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := manager.Submit(testPublishRequest(), "request-123")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("duplicate=%+v first=%+v", duplicate, first)
	}

	changed := testPublishRequest()
	changed.Content = "另一篇正文"
	if _, err := manager.Submit(changed, "request-123"); err == nil {
		t.Fatal("reusing one key for different content must fail")
	}
	close(release)
	waitTaskStatus(t, manager, first.TaskID, PublishTaskSucceeded)
}

func TestPublishTaskFallbackHashExpiresAfterTenMinutes(t *testing.T) {
	manager, err := NewPublishTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, *PublishRequest) (*PublishResponse, error) {
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()
	now := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	var unixNow atomic.Int64
	unixNow.Store(now.UnixNano())
	manager.now = func() time.Time { return time.Unix(0, unixNow.Load()) }

	first, err := manager.Submit(testPublishRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := manager.Submit(testPublishRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.TaskID != first.TaskID || !duplicate.Deduplicated {
		t.Fatalf("fallback duplicate=%+v first=%+v", duplicate, first)
	}

	now = now.Add(10*time.Minute + time.Second)
	unixNow.Store(now.UnixNano())
	afterWindow, err := manager.Submit(testPublishRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	if afterWindow.TaskID == first.TaskID || afterWindow.Deduplicated {
		t.Fatalf("task after window should be new: %+v", afterWindow)
	}
	waitTaskStatus(t, manager, afterWindow.TaskID, PublishTaskSucceeded)
}

func TestPublishTaskWorkerIsSerial(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	var active int32
	var maxActive int32
	manager, err := NewPublishTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(_ context.Context, req *PublishRequest) (*PublishResponse, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			seen := atomic.LoadInt32(&maxActive)
			if current <= seen || atomic.CompareAndSwapInt32(&maxActive, seen, current) {
				break
			}
		}
		started <- req.Title
		<-release
		atomic.AddInt32(&active, -1)
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()

	one := testPublishRequest()
	one.Title = "任务一"
	two := testPublishRequest()
	two.Title = "任务二"
	first, _ := manager.Submit(one, "one")
	second, _ := manager.Submit(two, "two")
	<-started
	select {
	case name := <-started:
		t.Fatalf("second task started concurrently: %s", name)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	<-started
	release <- struct{}{}
	waitTaskStatus(t, manager, first.TaskID, PublishTaskSucceeded)
	waitTaskStatus(t, manager, second.TaskID, PublishTaskSucceeded)
	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max active workers=%d, want 1", got)
	}
}

func TestPublishTaskRuntimeErrorIsUnknown(t *testing.T) {
	manager, err := NewPublishTaskManager(filepath.Join(t.TempDir(), "tasks.json"), func(context.Context, *PublishRequest) (*PublishResponse, error) {
		return nil, errors.New("browser result is ambiguous")
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()
	view, err := manager.Submit(testPublishRequest(), "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitTaskStatus(t, manager, view.TaskID, PublishTaskUnknown)
	if finished.Error == "" {
		t.Fatal("unknown task should explain why")
	}
}

func TestPublishTaskRestartMarksRunningUnknownAndResumesQueued(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	now := time.Now().UTC()
	stored := publishTaskFile{Version: 1, Tasks: map[string]*PublishTask{
		"running": {
			ID: "running", Status: PublishTaskRunning, IdempotencyKey: "running-key",
			IdempotencySource: "caller_key", RequestHash: "one", Request: testPublishRequest(), CreatedAt: now, UpdatedAt: now,
		},
		"queued": {
			ID: "queued", Status: PublishTaskQueued, IdempotencyKey: "queued-key",
			IdempotencySource: "caller_key", RequestHash: "two", Request: testPublishRequest(), CreatedAt: now, UpdatedAt: now,
		},
	}}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewPublishTaskManager(path, func(context.Context, *PublishRequest) (*PublishResponse, error) {
		return &PublishResponse{Status: "发布完成", Images: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.Start()
	waitTaskStatus(t, manager, "running", PublishTaskUnknown)
	waitTaskStatus(t, manager, "queued", PublishTaskSucceeded)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("task file mode=%o, want 600", info.Mode().Perm())
	}
}
