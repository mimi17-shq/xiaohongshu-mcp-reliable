package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const commentFallbackIdempotencyWindow = 10 * time.Minute

type CommentTaskStatus string

const (
	CommentTaskQueued    CommentTaskStatus = "queued"
	CommentTaskRunning   CommentTaskStatus = "running"
	CommentTaskConfirmed CommentTaskStatus = "confirmed"
	CommentTaskUnknown   CommentTaskStatus = "unknown"
)

type CommentTaskRequest struct {
	FeedID    string `json:"feed_id"`
	XsecToken string `json:"xsec_token"`
	Content   string `json:"content"`
}

type CommentTaskResult struct {
	FeedID  string `json:"feed_id"`
	Success bool   `json:"success"`
}

type CommentTask struct {
	ID                string              `json:"id"`
	Status            CommentTaskStatus   `json:"status"`
	IdempotencyKey    string              `json:"idempotency_key,omitempty"`
	IdempotencySource string              `json:"idempotency_source"`
	RequestHash       string              `json:"request_hash"`
	FeedID            string              `json:"feed_id"`
	Request           *CommentTaskRequest `json:"request,omitempty"`
	Result            *CommentTaskResult  `json:"result,omitempty"`
	Error             string              `json:"error,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
	StartedAt         *time.Time          `json:"started_at,omitempty"`
	FinishedAt        *time.Time          `json:"finished_at,omitempty"`
}

type CommentTaskView struct {
	TaskID            string             `json:"task_id"`
	Status            CommentTaskStatus  `json:"status"`
	FeedID            string             `json:"feed_id"`
	Deduplicated      bool               `json:"deduplicated,omitempty"`
	IdempotencySource string             `json:"idempotency_source"`
	Result            *CommentTaskResult `json:"result,omitempty"`
	Error             string             `json:"error,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

type CommentTaskLookup struct {
	MatchCount int               `json:"match_count"`
	Tasks      []CommentTaskView `json:"tasks"`
}

type commentTaskFile struct {
	Version int                     `json:"version"`
	Tasks   map[string]*CommentTask `json:"tasks"`
}

type commentExecutor func(context.Context, string, string, string) (*PostCommentResponse, error)

type CommentTaskManager struct {
	mu        sync.Mutex
	tasks     map[string]*CommentTask
	queue     chan string
	path      string
	execute   commentExecutor
	now       func() time.Time
	startOnce sync.Once
	startErr  error
}

func defaultCommentTaskPath() string {
	if path := strings.TrimSpace(os.Getenv("XHS_COMMENT_TASK_FILE")); path != "" {
		return path
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "xiaohongshu-mcp-comment-tasks.json")
	}
	return filepath.Join(configDir, "xhs-mcp", "runtime", "comment-tasks.json")
}

func NewCommentTaskManager(path string, execute commentExecutor) (*CommentTaskManager, error) {
	if execute == nil {
		return nil, errors.New("comment executor is required")
	}
	return &CommentTaskManager{
		tasks: make(map[string]*CommentTask), queue: make(chan string, 256),
		path: path, execute: execute, now: time.Now,
	}, nil
}

func (m *CommentTaskManager) Start() error {
	m.startOnce.Do(func() {
		queued, err := m.load()
		if err != nil {
			m.startErr = err
			return
		}
		go m.worker()
		for _, id := range queued {
			m.queue <- id
		}
	})
	return m.startErr
}

func (m *CommentTaskManager) Submit(req *CommentTaskRequest, idempotencyKey string) (CommentTaskView, error) {
	if err := validateCommentTaskRequest(req); err != nil {
		return CommentTaskView{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) > 128 {
		return CommentTaskView{}, errors.New("idempotency_key 最多 128 个字符")
	}
	hash, err := hashCommentTaskRequest(req)
	if err != nil {
		return CommentTaskView{}, err
	}

	m.mu.Lock()
	now := m.now().UTC()
	if existing, conflict := m.findDuplicateLocked(idempotencyKey, hash, now); existing != nil {
		if conflict {
			m.mu.Unlock()
			return CommentTaskView{}, errors.New("同一个 idempotency_key 已用于不同的顶层评论请求")
		}
		view := commentTaskView(existing, true)
		m.mu.Unlock()
		return view, nil
	}
	id, err := newCommentTaskID()
	if err != nil {
		m.mu.Unlock()
		return CommentTaskView{}, err
	}
	source := "caller_key"
	if idempotencyKey == "" {
		source = "request_hash_10m"
	}
	task := &CommentTask{
		ID: id, Status: CommentTaskQueued, IdempotencyKey: idempotencyKey,
		IdempotencySource: source, RequestHash: hash, FeedID: req.FeedID,
		Request: cloneCommentTaskRequest(req), CreatedAt: now, UpdatedAt: now,
	}
	m.tasks[id] = task
	if err := m.persistLocked(); err != nil {
		delete(m.tasks, id)
		m.mu.Unlock()
		return CommentTaskView{}, fmt.Errorf("保存顶层评论任务失败: %w", err)
	}
	view := commentTaskView(task, false)
	m.mu.Unlock()
	m.queue <- id
	return view, nil
}

func (m *CommentTaskManager) Get(id string) (CommentTaskView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[strings.TrimSpace(id)]
	if !ok {
		return CommentTaskView{}, false
	}
	return commentTaskView(task, false), true
}

func (m *CommentTaskManager) FindByFeed(feedID string) CommentTaskLookup {
	feedID = strings.TrimSpace(feedID)
	m.mu.Lock()
	defer m.mu.Unlock()
	var tasks []*CommentTask
	for _, task := range m.tasks {
		if task.FeedID == feedID {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	views := make([]CommentTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, commentTaskView(task, false))
	}
	return CommentTaskLookup{MatchCount: len(views), Tasks: views}
}

func (m *CommentTaskManager) findDuplicateLocked(key, hash string, now time.Time) (*CommentTask, bool) {
	for _, task := range m.tasks {
		if key != "" {
			if task.IdempotencyKey == key {
				return task, task.RequestHash != hash
			}
			continue
		}
		if task.IdempotencyKey == "" && task.RequestHash == hash && now.Sub(task.CreatedAt) <= commentFallbackIdempotencyWindow {
			return task, false
		}
	}
	return nil, false
}

func (m *CommentTaskManager) worker() {
	for id := range m.queue {
		m.run(id)
	}
}

func (m *CommentTaskManager) run(id string) {
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok || task.Status != CommentTaskQueued || task.Request == nil {
		m.mu.Unlock()
		return
	}
	now := m.now().UTC()
	task.Status = CommentTaskRunning
	task.StartedAt, task.UpdatedAt = &now, now
	request := cloneCommentTaskRequest(task.Request)
	if err := m.persistLocked(); err != nil {
		task.Status = CommentTaskUnknown
		task.Error = safeCommentTaskError(err)
		task.Request = nil
		task.FinishedAt = &now
		_ = m.persistLocked()
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	result, runErr, panicked := m.executeSafely(request)
	m.mu.Lock()
	task, ok = m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	finished := m.now().UTC()
	task.UpdatedAt, task.FinishedAt, task.Request = finished, &finished, nil
	if runErr != nil || panicked || result == nil || !result.Success {
		task.Status = CommentTaskUnknown
		task.Error = safeCommentTaskError(runErr)
	} else {
		task.Status = CommentTaskConfirmed
		task.Result = &CommentTaskResult{FeedID: result.FeedID, Success: true}
	}
	if err := m.persistLocked(); err != nil {
		logrus.Errorf("persist comment task %s: %v", id, err)
	}
	status := task.Status
	logrus.Infof("comment task completed: task_id=%s status=%s", id, status)
	m.mu.Unlock()
}

func (m *CommentTaskManager) executeSafely(req *CommentTaskRequest) (result *PostCommentResponse, err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("顶层评论执行异常: %v", recovered)
		}
	}()
	result, err = m.execute(context.Background(), req.FeedID, req.XsecToken, req.Content)
	return result, err, false
}

func (m *CommentTaskManager) load() ([]string, error) {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取顶层评论任务文件失败: %w", err)
	}
	var stored commentTaskFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("解析顶层评论任务文件失败: %w", err)
	}
	if stored.Tasks != nil {
		m.tasks = stored.Tasks
	}
	now := m.now().UTC()
	queued := make([]string, 0)
	changed := false
	for _, task := range m.tasks {
		switch task.Status {
		case CommentTaskQueued:
			if task.Request != nil {
				queued = append(queued, task.ID)
			}
		case CommentTaskRunning:
			task.Status = CommentTaskUnknown
			task.Error = "服务在顶层评论执行期间重启，无法确认是否已发送；请先核实，绝对不要盲目重发"
			task.Request = nil
			task.UpdatedAt, task.FinishedAt = now, &now
			changed = true
		}
	}
	if changed {
		if err := m.persistLocked(); err != nil {
			return nil, err
		}
	}
	return queued, nil
}

func (m *CommentTaskManager) persistLocked() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".comment-tasks-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(commentTaskFile{Version: 1, Tasks: m.tasks}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return err
	}
	return os.Chmod(m.path, 0o600)
}

func validateCommentTaskRequest(req *CommentTaskRequest) error {
	if req == nil {
		return errors.New("顶层评论请求不能为空")
	}
	if strings.TrimSpace(req.FeedID) == "" || strings.TrimSpace(req.XsecToken) == "" || strings.TrimSpace(req.Content) == "" {
		return errors.New("feed_id、xsec_token 和 content 均为必填")
	}
	return nil
}

func hashCommentTaskRequest(req *CommentTaskRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("生成顶层评论请求摘要失败: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func newCommentTaskID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "xhs_comment_" + hex.EncodeToString(buf), nil
}

func cloneCommentTaskRequest(req *CommentTaskRequest) *CommentTaskRequest {
	if req == nil {
		return nil
	}
	clone := *req
	return &clone
}

func commentTaskView(task *CommentTask, deduplicated bool) CommentTaskView {
	return CommentTaskView{
		TaskID: task.ID, Status: task.Status, FeedID: task.FeedID,
		Deduplicated: deduplicated, IdempotencySource: task.IdempotencySource,
		Result: task.Result, Error: task.Error, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
}

func safeCommentTaskError(err error) string {
	if err == nil {
		return "顶层评论流程异常结束，最终结果无法确认；请先核实，绝对不要盲目重发"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 500
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}
