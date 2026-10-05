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
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

const (
	replyFallbackIdempotencyWindow = 10 * time.Minute
	replyTargetCooldownWindow      = 10 * time.Minute
)

type ReplyTaskStatus string

const (
	ReplyTaskQueued      ReplyTaskStatus = "queued"
	ReplyTaskRunning     ReplyTaskStatus = "running"
	ReplyTaskConfirmed   ReplyTaskStatus = "confirmed"
	ReplyTaskRejected    ReplyTaskStatus = "rejected"
	ReplyTaskBlocked     ReplyTaskStatus = "blocked"
	ReplyTaskUncertain   ReplyTaskStatus = "uncertain"
	ReplyTaskWrongTarget ReplyTaskStatus = "wrong_target"
)

type ReplyTaskRequest struct {
	FeedID    string `json:"feed_id"`
	XsecToken string `json:"xsec_token"`
	CommentID string `json:"comment_id"`
	UserID    string `json:"user_id,omitempty"`
	Content   string `json:"content"`
}

type ReplyTask struct {
	ID                string                         `json:"id"`
	Status            ReplyTaskStatus                `json:"status"`
	RequestStatus     xiaohongshu.ReplyRequestStatus `json:"request_status"`
	ServerStatus      xiaohongshu.ReplyServerStatus  `json:"server_status"`
	IdempotencyKey    string                         `json:"idempotency_key,omitempty"`
	IdempotencySource string                         `json:"idempotency_source"`
	RequestHash       string                         `json:"request_hash"`
	FeedID            string                         `json:"feed_id"`
	CommentID         string                         `json:"comment_id"`
	Phase             xiaohongshu.ReplyProgressPhase `json:"phase"`
	PhaseUpdatedAt    time.Time                      `json:"phase_updated_at"`
	Request           *ReplyTaskRequest              `json:"request,omitempty"`
	Result            *ReplyCommentResponse          `json:"result,omitempty"`
	Error             string                         `json:"error,omitempty"`
	CreatedAt         time.Time                      `json:"created_at"`
	UpdatedAt         time.Time                      `json:"updated_at"`
	StartedAt         *time.Time                     `json:"started_at,omitempty"`
	FinishedAt        *time.Time                     `json:"finished_at,omitempty"`
}

type ReplyTaskView struct {
	TaskID              string                         `json:"task_id"`
	Status              ReplyTaskStatus                `json:"status"`
	RequestStatus       xiaohongshu.ReplyRequestStatus `json:"request_status"`
	ServerStatus        xiaohongshu.ReplyServerStatus  `json:"server_status"`
	FeedID              string                         `json:"feed_id"`
	CommentID           string                         `json:"comment_id"`
	Phase               xiaohongshu.ReplyProgressPhase `json:"phase"`
	PhaseUpdatedAt      time.Time                      `json:"phase_updated_at"`
	ReplyButtonAttempts int                            `json:"reply_button_attempts,omitempty"`
	Deduplicated        bool                           `json:"deduplicated,omitempty"`
	IdempotencySource   string                         `json:"idempotency_source"`
	Result              *ReplyCommentResponse          `json:"result,omitempty"`
	Error               string                         `json:"error,omitempty"`
	CreatedAt           time.Time                      `json:"created_at"`
	UpdatedAt           time.Time                      `json:"updated_at"`
}

type ReplyTaskLookup struct {
	MatchCount int             `json:"match_count"`
	Tasks      []ReplyTaskView `json:"tasks"`
}

type replyTaskFile struct {
	Version int                   `json:"version"`
	Tasks   map[string]*ReplyTask `json:"tasks"`
}

type replyProgress = xiaohongshu.ReplyProgressFunc

type replyExecutor func(context.Context, string, string, string, string, string, replyProgress) (*ReplyCommentResponse, error)

type ReplyTaskManager struct {
	mu        sync.Mutex
	tasks     map[string]*ReplyTask
	queue     chan string
	path      string
	execute   replyExecutor
	now       func() time.Time
	startOnce sync.Once
	startErr  error
}

func defaultReplyTaskPath() string {
	if path := strings.TrimSpace(os.Getenv("XHS_REPLY_TASK_FILE")); path != "" {
		return path
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "xiaohongshu-mcp-reply-tasks.json")
	}
	return filepath.Join(configDir, "xhs-mcp", "runtime", "reply-tasks.json")
}

func NewReplyTaskManager(path string, execute replyExecutor) (*ReplyTaskManager, error) {
	if execute == nil {
		return nil, errors.New("reply executor is required")
	}
	return &ReplyTaskManager{
		tasks: make(map[string]*ReplyTask), queue: make(chan string, 256),
		path: path, execute: execute, now: time.Now,
	}, nil
}

func (m *ReplyTaskManager) Start() error {
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

func (m *ReplyTaskManager) Submit(req *ReplyTaskRequest, idempotencyKey string) (ReplyTaskView, error) {
	if err := validateReplyTaskRequest(req); err != nil {
		return ReplyTaskView{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) > 128 {
		return ReplyTaskView{}, errors.New("idempotency_key 最多 128 个字符")
	}
	hash, err := hashReplyTaskRequest(req)
	if err != nil {
		return ReplyTaskView{}, err
	}

	m.mu.Lock()
	now := m.now().UTC()
	if existing, conflict := m.findDuplicateLocked(idempotencyKey, hash, now); existing != nil {
		if conflict {
			m.mu.Unlock()
			return ReplyTaskView{}, errors.New("同一个 idempotency_key 已用于不同的回复请求")
		}
		view := replyTaskView(existing, true)
		m.mu.Unlock()
		return view, nil
	}
	if existing := m.findTargetCooldownLocked(req.FeedID, req.CommentID, now); existing != nil {
		m.mu.Unlock()
		return ReplyTaskView{}, fmt.Errorf(
			"同一 feed_id+comment_id 近期已有任务 %s（status=%s）；为避免改写正文或更换幂等键造成重复回复，本次未创建新任务，请先查询原任务",
			existing.ID, existing.Status,
		)
	}
	id, err := newReplyTaskID()
	if err != nil {
		m.mu.Unlock()
		return ReplyTaskView{}, err
	}
	source := "caller_key"
	if idempotencyKey == "" {
		source = "request_hash_10m"
	}
	task := &ReplyTask{
		ID: id, Status: ReplyTaskQueued, RequestStatus: xiaohongshu.ReplyRequestNotStarted, ServerStatus: xiaohongshu.ReplyServerNotObserved,
		IdempotencyKey: idempotencyKey, IdempotencySource: source, RequestHash: hash,
		FeedID: req.FeedID, CommentID: req.CommentID, Request: cloneReplyTaskRequest(req),
		Phase: xiaohongshu.ReplyPhaseQueued, PhaseUpdatedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	m.tasks[id] = task
	if err := m.persistLocked(); err != nil {
		delete(m.tasks, id)
		m.mu.Unlock()
		return ReplyTaskView{}, fmt.Errorf("保存回复任务失败: %w", err)
	}
	view := replyTaskView(task, false)
	m.mu.Unlock()
	m.queue <- id
	return view, nil
}

func (m *ReplyTaskManager) Get(id string) (ReplyTaskView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[strings.TrimSpace(id)]
	if !ok {
		return ReplyTaskView{}, false
	}
	return replyTaskView(task, false), true
}

func (m *ReplyTaskManager) FindByTarget(feedID, commentID string) ReplyTaskLookup {
	feedID, commentID = strings.TrimSpace(feedID), strings.TrimSpace(commentID)
	m.mu.Lock()
	defer m.mu.Unlock()
	var tasks []*ReplyTask
	for _, task := range m.tasks {
		if task.FeedID == feedID && task.CommentID == commentID {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	views := make([]ReplyTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, replyTaskView(task, false))
	}
	return ReplyTaskLookup{MatchCount: len(views), Tasks: views}
}

func (m *ReplyTaskManager) findDuplicateLocked(key, hash string, now time.Time) (*ReplyTask, bool) {
	for _, task := range m.tasks {
		if key != "" {
			if task.IdempotencyKey == key {
				return task, task.RequestHash != hash
			}
			continue
		}
		if task.IdempotencyKey == "" && task.RequestHash == hash && now.Sub(task.CreatedAt) <= replyFallbackIdempotencyWindow {
			return task, false
		}
	}
	return nil, false
}

func (m *ReplyTaskManager) findTargetCooldownLocked(feedID, commentID string, now time.Time) *ReplyTask {
	feedID, commentID = strings.TrimSpace(feedID), strings.TrimSpace(commentID)
	var newest *ReplyTask
	for _, task := range m.tasks {
		if task.FeedID != feedID || task.CommentID != commentID {
			continue
		}
		blocks := task.Status == ReplyTaskQueued || task.Status == ReplyTaskRunning
		if !blocks && now.Sub(task.CreatedAt) <= replyTargetCooldownWindow {
			blocks = task.Status == ReplyTaskUncertain || task.Status == ReplyTaskConfirmed
		}
		if blocks && (newest == nil || task.CreatedAt.After(newest.CreatedAt)) {
			newest = task
		}
	}
	return newest
}

func (m *ReplyTaskManager) worker() {
	for id := range m.queue {
		m.run(id)
	}
}

func (m *ReplyTaskManager) run(id string) {
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok || task.Status != ReplyTaskQueued || task.Request == nil {
		m.mu.Unlock()
		return
	}
	now := m.now().UTC()
	task.Status = ReplyTaskRunning
	task.RequestStatus = xiaohongshu.ReplyRequestNotStarted
	task.StartedAt, task.UpdatedAt = &now, now
	request := cloneReplyTaskRequest(task.Request)
	if err := m.persistLocked(); err != nil {
		task.Status = ReplyTaskBlocked
		task.Error = safeReplyTaskError(fmt.Errorf("无法在执行前持久化 running 状态: %w", err))
		task.Request = nil
		task.FinishedAt = &now
		_ = m.persistLocked()
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	result, runErr, panicked := m.executeSafely(id, request)
	m.mu.Lock()
	task, ok = m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	finished := m.now().UTC()
	task.UpdatedAt, task.FinishedAt, task.Request = finished, &finished, nil
	if result != nil {
		task.Result = result
		task.RequestStatus = result.RequestStatus
		task.ServerStatus = result.ServerStatus
		task.Status = replyTaskStatusFromVerification(result.VerificationStatus)
		task.Error = safeReplyTaskError(runErr)
	} else {
		// No structured result means the browser may have crossed the side-effect
		// boundary before failing. Never turn this into a retryable failure.
		task.Status = ReplyTaskUncertain
		task.RequestStatus = xiaohongshu.ReplyRequestNotObserved
		task.Error = safeReplyTaskError(runErr)
		if panicked && runErr == nil {
			task.Error = "回复执行异常，最终结果无法确认；请勿盲目重发"
		}
	}
	if err := m.persistLocked(); err != nil {
		logrus.Errorf("persist reply task %s: %v", id, err)
	}
	status := task.Status
	m.mu.Unlock()
	logrus.Infof("reply task completed: task_id=%s status=%s request_status=%s", id, status, task.RequestStatus)
}

func (m *ReplyTaskManager) executeSafely(id string, req *ReplyTaskRequest) (result *ReplyCommentResponse, err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("回复执行异常: %v", recovered)
		}
	}()
	result, err = m.execute(context.Background(), req.FeedID, req.XsecToken, req.CommentID, req.UserID, req.Content,
		func(status xiaohongshu.ReplyVerificationStatus, requestStatus xiaohongshu.ReplyRequestStatus, serverStatus xiaohongshu.ReplyServerStatus, phase xiaohongshu.ReplyProgressPhase) {
			m.recordProgress(id, status, requestStatus, serverStatus, phase)
		})
	return result, err, false
}

func (m *ReplyTaskManager) recordProgress(id string, _ xiaohongshu.ReplyVerificationStatus, requestStatus xiaohongshu.ReplyRequestStatus, serverStatus xiaohongshu.ReplyServerStatus, phase xiaohongshu.ReplyProgressPhase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok || task.Status != ReplyTaskRunning {
		return
	}
	changed := false
	if requestStatus != "" && task.RequestStatus != requestStatus {
		task.RequestStatus = requestStatus
		changed = true
	}
	if serverStatus != "" && task.ServerStatus != serverStatus {
		task.ServerStatus = serverStatus
		changed = true
	}
	now := m.now().UTC()
	if phase != "" && phase != task.Phase && replyPhaseCanAdvance(task.Phase, phase) {
		task.Phase = phase
		task.PhaseUpdatedAt = now
		changed = true
	}
	if !changed {
		return
	}
	task.UpdatedAt = now
	if err := m.persistLocked(); err != nil {
		logrus.Errorf("persist reply task progress %s: %v", id, err)
	}
}

func (m *ReplyTaskManager) load() ([]string, error) {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取回复任务文件失败: %w", err)
	}
	var stored replyTaskFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("解析回复任务文件失败: %w", err)
	}
	if stored.Tasks != nil {
		m.tasks = stored.Tasks
	}
	now := m.now().UTC()
	queued := make([]string, 0)
	changed := false
	for _, task := range m.tasks {
		if task.Phase == "" {
			if task.Status == ReplyTaskQueued {
				task.Phase = xiaohongshu.ReplyPhaseQueued
			} else {
				task.Phase = xiaohongshu.ReplyPhaseUnknown
			}
			changed = true
		}
		if task.PhaseUpdatedAt.IsZero() {
			task.PhaseUpdatedAt = task.UpdatedAt
			changed = true
		}
		if task.ServerStatus == "" {
			if task.Status == ReplyTaskQueued {
				task.ServerStatus = xiaohongshu.ReplyServerNotObserved
			} else {
				task.ServerStatus = xiaohongshu.ReplyServerUnknown
			}
			changed = true
		}
		switch task.Status {
		case ReplyTaskQueued:
			if task.Request != nil {
				queued = append(queued, task.ID)
			}
		case ReplyTaskRunning:
			// A crash can occur after the browser sent the request but before the
			// final state was persisted. Preserve a phase already proven by the
			// network gate; otherwise downgrade to not_observed, never not_started.
			switch {
			case task.ServerStatus == xiaohongshu.ReplyServerRejected:
				task.Status = ReplyTaskRejected
				task.Error = "服务在回复执行期间重启，但已持久化小红书服务器明确拒绝该请求，确定未发布成功"
			case task.RequestStatus == xiaohongshu.ReplyRequestBlocked:
				task.Status = ReplyTaskBlocked
				task.Error = "服务在回复执行期间重启，但网络闸门已记录请求在离开浏览器前被阻断，确定未发送"
			case task.RequestStatus == xiaohongshu.ReplyRequestAllowed:
				task.Status = ReplyTaskUncertain
				task.Error = "服务在已向正确目标放行回复请求后重启，最终结果无法确认；绝对不要重发"
			default:
				task.Status = ReplyTaskUncertain
				task.RequestStatus = xiaohongshu.ReplyRequestNotObserved
				task.Error = "服务在回复执行期间重启，无法确认请求是否已发出；请查询评论树，绝对不要盲目重发"
			}
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

func (m *ReplyTaskManager) persistLocked() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".reply-tasks-*")
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
	if err := enc.Encode(replyTaskFile{Version: 1, Tasks: m.tasks}); err != nil {
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

func validateReplyTaskRequest(req *ReplyTaskRequest) error {
	if req == nil {
		return errors.New("回复请求不能为空")
	}
	if strings.TrimSpace(req.FeedID) == "" || strings.TrimSpace(req.XsecToken) == "" || strings.TrimSpace(req.CommentID) == "" || strings.TrimSpace(req.Content) == "" {
		return errors.New("feed_id、xsec_token、comment_id 和 content 均为必填")
	}
	return nil
}

func hashReplyTaskRequest(req *ReplyTaskRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("生成回复请求摘要失败: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func newReplyTaskID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成回复任务 ID 失败: %w", err)
	}
	return "xhs_reply_" + hex.EncodeToString(buf), nil
}

func cloneReplyTaskRequest(req *ReplyTaskRequest) *ReplyTaskRequest {
	if req == nil {
		return nil
	}
	clone := *req
	return &clone
}

func replyTaskStatusFromVerification(status xiaohongshu.ReplyVerificationStatus) ReplyTaskStatus {
	switch status {
	case xiaohongshu.ReplyConfirmed:
		return ReplyTaskConfirmed
	case xiaohongshu.ReplyRejected:
		return ReplyTaskRejected
	case xiaohongshu.ReplyBlocked:
		return ReplyTaskBlocked
	case xiaohongshu.ReplyWrongTarget:
		return ReplyTaskWrongTarget
	default:
		return ReplyTaskUncertain
	}
}

func replyTaskView(task *ReplyTask, deduplicated bool) ReplyTaskView {
	buttonAttempts := 0
	if task.Result != nil {
		buttonAttempts = task.Result.ReplyButtonAttempts
	}
	return ReplyTaskView{
		TaskID: task.ID, Status: task.Status, RequestStatus: task.RequestStatus, ServerStatus: task.ServerStatus,
		FeedID: task.FeedID, CommentID: task.CommentID, Phase: task.Phase, PhaseUpdatedAt: task.PhaseUpdatedAt,
		ReplyButtonAttempts: buttonAttempts, Deduplicated: deduplicated,
		IdempotencySource: task.IdempotencySource, Result: task.Result, Error: task.Error,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
}

func replyPhaseCanAdvance(current, next xiaohongshu.ReplyProgressPhase) bool {
	rank := func(phase xiaohongshu.ReplyProgressPhase) (int, bool) {
		switch phase {
		case xiaohongshu.ReplyPhaseQueued:
			return 0, true
		case xiaohongshu.ReplyPhaseLocatingComment:
			return 1, true
		case xiaohongshu.ReplyPhaseTargetLocated:
			return 2, true
		case xiaohongshu.ReplyPhaseReplyEditorOpen:
			return 3, true
		case xiaohongshu.ReplyPhaseReplyContentEntered:
			return 4, true
		case xiaohongshu.ReplyPhaseSubmittedPendingVerification:
			return 5, true
		default:
			return -1, false
		}
	}
	nextRank, nextOK := rank(next)
	if !nextOK {
		return false
	}
	currentRank, currentOK := rank(current)
	return !currentOK || nextRank > currentRank
}

func safeReplyTaskError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 500
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}
