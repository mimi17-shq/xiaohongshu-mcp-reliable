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
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/downloader"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/xhsutil"
)

const fallbackIdempotencyWindow = 10 * time.Minute

type PublishTaskStatus string

const (
	PublishTaskQueued    PublishTaskStatus = "queued"
	PublishTaskRunning   PublishTaskStatus = "running"
	PublishTaskSucceeded PublishTaskStatus = "succeeded"
	PublishTaskFailed    PublishTaskStatus = "failed"
	PublishTaskUnknown   PublishTaskStatus = "unknown"
)

type PublishTaskResult struct {
	Status string `json:"status"`
	Images int    `json:"images"`
}

// PublishTask is the durable server-side record. Request is retained only while
// a task is queued or running, then removed so completed task files do not keep
// post bodies or image paths.
type PublishTask struct {
	ID                string             `json:"id"`
	Status            PublishTaskStatus  `json:"status"`
	IdempotencyKey    string             `json:"idempotency_key,omitempty"`
	IdempotencySource string             `json:"idempotency_source"`
	RequestHash       string             `json:"request_hash"`
	Request           *PublishRequest    `json:"request,omitempty"`
	Result            *PublishTaskResult `json:"result,omitempty"`
	Error             string             `json:"error,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	StartedAt         *time.Time         `json:"started_at,omitempty"`
	FinishedAt        *time.Time         `json:"finished_at,omitempty"`
}

// PublishTaskView is safe to return to callers. It intentionally omits the
// original request, content hash and caller-provided idempotency key.
type PublishTaskView struct {
	TaskID            string             `json:"task_id"`
	Status            PublishTaskStatus  `json:"status"`
	Deduplicated      bool               `json:"deduplicated,omitempty"`
	IdempotencySource string             `json:"idempotency_source"`
	Result            *PublishTaskResult `json:"result,omitempty"`
	Error             string             `json:"error,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

type publishTaskFile struct {
	Version int                     `json:"version"`
	Tasks   map[string]*PublishTask `json:"tasks"`
}

type publishExecutor func(context.Context, *PublishRequest) (*PublishResponse, error)

type PublishTaskManager struct {
	mu        sync.Mutex
	tasks     map[string]*PublishTask
	queue     chan string
	path      string
	execute   publishExecutor
	now       func() time.Time
	startOnce sync.Once
	startErr  error
	recovered []string
}

func defaultPublishTaskPath() string {
	if path := strings.TrimSpace(os.Getenv("XHS_PUBLISH_TASK_FILE")); path != "" {
		return path
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "xiaohongshu-mcp-publish-tasks.json")
	}
	return filepath.Join(configDir, "xhs-mcp", "runtime", "publish-tasks.json")
}

func NewPublishTaskManager(path string, execute publishExecutor) (*PublishTaskManager, error) {
	if execute == nil {
		return nil, errors.New("publish executor is required")
	}
	m := &PublishTaskManager{
		tasks:   make(map[string]*PublishTask),
		queue:   make(chan string, 256),
		path:    path,
		execute: execute,
		now:     time.Now,
	}
	return m, nil
}

func (m *PublishTaskManager) Start() error {
	m.startOnce.Do(func() {
		queued, err := m.load()
		if err != nil {
			m.startErr = err
			return
		}
		m.recovered = queued
		go m.worker()
		for _, id := range m.recovered {
			m.queue <- id
		}
		m.recovered = nil
	})
	return m.startErr
}

func (m *PublishTaskManager) Submit(req *PublishRequest, idempotencyKey string) (PublishTaskView, error) {
	if err := validatePublishRequest(req); err != nil {
		return PublishTaskView{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) > 128 {
		return PublishTaskView{}, errors.New("idempotency_key 最多 128 个字符")
	}
	hash, err := hashPublishRequest(req)
	if err != nil {
		return PublishTaskView{}, err
	}

	m.mu.Lock()
	now := m.now().UTC()
	if existing, conflict := m.findDuplicateLocked(idempotencyKey, hash, now); existing != nil {
		if conflict {
			m.mu.Unlock()
			return PublishTaskView{}, errors.New("同一个 idempotency_key 已用于不同的发布内容")
		}
		view := taskView(existing, true)
		m.mu.Unlock()
		return view, nil
	}
	if err := validateLocalPublishImages(req.Images); err != nil {
		m.mu.Unlock()
		return PublishTaskView{}, err
	}

	id, err := newPublishTaskID()
	if err != nil {
		m.mu.Unlock()
		return PublishTaskView{}, err
	}
	source := "caller_key"
	if idempotencyKey == "" {
		source = "content_hash_10m"
	}
	task := &PublishTask{
		ID:                id,
		Status:            PublishTaskQueued,
		IdempotencyKey:    idempotencyKey,
		IdempotencySource: source,
		RequestHash:       hash,
		Request:           clonePublishRequest(req),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	m.tasks[id] = task
	if err := m.persistLocked(); err != nil {
		delete(m.tasks, id)
		m.mu.Unlock()
		return PublishTaskView{}, fmt.Errorf("保存发布任务失败: %w", err)
	}
	view := taskView(task, false)
	m.mu.Unlock()

	m.queue <- id
	return view, nil
}

func (m *PublishTaskManager) Get(id string) (PublishTaskView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[strings.TrimSpace(id)]
	if !ok {
		return PublishTaskView{}, false
	}
	return taskView(task, false), true
}

func (m *PublishTaskManager) findDuplicateLocked(key, hash string, now time.Time) (*PublishTask, bool) {
	for _, task := range m.tasks {
		if key != "" {
			if task.IdempotencyKey == key {
				return task, task.RequestHash != hash
			}
			continue
		}
		if task.IdempotencyKey == "" && task.RequestHash == hash && now.Sub(task.CreatedAt) <= fallbackIdempotencyWindow {
			return task, false
		}
	}
	return nil, false
}

func (m *PublishTaskManager) worker() {
	for id := range m.queue {
		m.run(id)
	}
}

func (m *PublishTaskManager) run(id string) {
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok || task.Status != PublishTaskQueued || task.Request == nil {
		m.mu.Unlock()
		return
	}
	now := m.now().UTC()
	task.Status = PublishTaskRunning
	task.StartedAt = &now
	task.UpdatedAt = now
	request := clonePublishRequest(task.Request)
	if err := m.persistLocked(); err != nil {
		task.Status = PublishTaskFailed
		task.Error = safeTaskError(fmt.Errorf("无法在执行前持久化 running 状态: %w", err))
		task.Request = nil
		task.FinishedAt = &now
		_ = m.persistLocked()
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	result, runErr, panicked := m.executeSafely(request)
	if runErr == nil && result == nil {
		runErr = errors.New("发布执行器未返回结果")
	}

	m.mu.Lock()
	task, ok = m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	finished := m.now().UTC()
	task.UpdatedAt = finished
	task.FinishedAt = &finished
	task.Request = nil
	if runErr != nil || panicked {
		// Browser automation can fail after the site accepted a publish click.
		// Without a definitive success marker, "failed" would invite duplicates.
		task.Status = PublishTaskUnknown
		task.Error = safeTaskError(runErr)
	} else {
		task.Status = PublishTaskSucceeded
		task.Result = &PublishTaskResult{Status: result.Status, Images: result.Images}
	}
	if err := m.persistLocked(); err != nil {
		logrus.Errorf("persist publish task %s: %v", id, err)
	}
	status := task.Status
	m.mu.Unlock()
	logrus.Infof("publish task completed: task_id=%s status=%s", id, status)
}

func (m *PublishTaskManager) executeSafely(req *PublishRequest) (result *PublishResponse, err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("发布执行异常: %v", recovered)
		}
	}()
	result, err = m.execute(context.Background(), req)
	return result, err, false
}

func (m *PublishTaskManager) load() ([]string, error) {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取发布任务文件失败: %w", err)
	}
	var stored publishTaskFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("解析发布任务文件失败: %w", err)
	}
	if stored.Tasks != nil {
		m.tasks = stored.Tasks
	}
	now := m.now().UTC()
	queued := make([]string, 0)
	changed := false
	for _, task := range m.tasks {
		switch task.Status {
		case PublishTaskQueued:
			if task.Request != nil {
				queued = append(queued, task.ID)
			}
		case PublishTaskRunning:
			// A restart during a side effect cannot prove whether XHS accepted it.
			task.Status = PublishTaskUnknown
			task.Error = "服务在发布执行期间重启，最终结果无法确认；请勿使用相同内容盲目重发"
			task.Request = nil
			task.UpdatedAt = now
			task.FinishedAt = &now
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

func (m *PublishTaskManager) persistLocked() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".publish-tasks-*")
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
	if err := enc.Encode(publishTaskFile{Version: 1, Tasks: m.tasks}); err != nil {
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

func validatePublishRequest(req *PublishRequest) error {
	if req == nil {
		return errors.New("发布请求不能为空")
	}
	if strings.TrimSpace(req.Title) == "" {
		return errors.New("标题不能为空")
	}
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return errors.New("标题长度超过限制")
	}
	if strings.TrimSpace(req.Content) == "" {
		return errors.New("正文不能为空")
	}
	if len(req.Images) == 0 {
		return errors.New("至少需要一张图片")
	}
	if len(req.ScheduleAt) > 0 {
		scheduled, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %w", err)
		}
		now := time.Now()
		if scheduled.Before(now.Add(time.Hour)) {
			return errors.New("定时发布时间必须至少在1小时后")
		}
		if scheduled.After(now.Add(14 * 24 * time.Hour)) {
			return errors.New("定时发布时间不能超过14天")
		}
	}
	return nil
}

func validateLocalPublishImages(images []string) error {
	for i, image := range images {
		if downloader.IsImageURL(image) {
			continue
		}
		if err := validateReadableRegularFile(image); err != nil {
			return fmt.Errorf("第%d张本地图片不可读: %w", i+1, err)
		}
	}
	return nil
}

func validateReadableRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("不是普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func hashPublishRequest(req *PublishRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("生成发布请求摘要失败: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func newPublishTaskID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成任务 ID 失败: %w", err)
	}
	return "xhs_" + hex.EncodeToString(buf), nil
}

func clonePublishRequest(req *PublishRequest) *PublishRequest {
	if req == nil {
		return nil
	}
	clone := *req
	clone.Images = append([]string(nil), req.Images...)
	clone.Tags = append([]string(nil), req.Tags...)
	clone.Products = append([]string(nil), req.Products...)
	return &clone
}

func taskView(task *PublishTask, deduplicated bool) PublishTaskView {
	return PublishTaskView{
		TaskID:            task.ID,
		Status:            task.Status,
		Deduplicated:      deduplicated,
		IdempotencySource: task.IdempotencySource,
		Result:            task.Result,
		Error:             task.Error,
		CreatedAt:         task.CreatedAt,
		UpdatedAt:         task.UpdatedAt,
	}
}

func safeTaskError(err error) string {
	if err == nil {
		return "发布流程异常结束，最终结果无法确认"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 500
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}
