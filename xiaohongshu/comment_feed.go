// Modified from upstream: adds deterministic reply targeting and result verification.
package xiaohongshu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// CommentFeedAction 表示 Feed 评论动作
type CommentFeedAction struct {
	page *rod.Page
}

// ReplyVerificationStatus 是回复提交后的机械验收结果。
type ReplyVerificationStatus string

const (
	ReplyConfirmed   ReplyVerificationStatus = "confirmed"
	ReplyRejected    ReplyVerificationStatus = "rejected"
	ReplyUncertain   ReplyVerificationStatus = "uncertain"
	ReplyWrongTarget ReplyVerificationStatus = "wrong_target"
	ReplyBlocked     ReplyVerificationStatus = "blocked"
)

// ReplyServerStatus records whether XHS itself accepted the released POST.
// It is separate from RequestStatus: allowed_to_target only proves that the
// browser gate released a correctly targeted request.
type ReplyServerStatus string

const (
	ReplyServerNotObserved ReplyServerStatus = "not_observed"
	ReplyServerAccepted    ReplyServerStatus = "accepted"
	ReplyServerRejected    ReplyServerStatus = "rejected"
	ReplyServerUnknown     ReplyServerStatus = "unknown"
)

// ReplyRequestStatus 区分请求是否曾被允许离开浏览器。
type ReplyRequestStatus string

const (
	ReplyRequestNotStarted  ReplyRequestStatus = "not_started"
	ReplyRequestBlocked     ReplyRequestStatus = "blocked_before_network"
	ReplyRequestAllowed     ReplyRequestStatus = "allowed_to_target"
	ReplyRequestNotObserved ReplyRequestStatus = "not_observed"
)

// ReplyProgressPhase 是异步回复可持久化的粗粒度阶段。除 queued 外，
// 每个值只在对应的真实页面或网络事件发生时上报一次，不做定时心跳。
type ReplyProgressPhase string

const (
	ReplyPhaseUnknown                      ReplyProgressPhase = "unknown"
	ReplyPhaseQueued                       ReplyProgressPhase = "queued"
	ReplyPhaseLocatingComment              ReplyProgressPhase = "locating_comment"
	ReplyPhaseTargetLocated                ReplyProgressPhase = "target_located"
	ReplyPhaseReplyEditorOpen              ReplyProgressPhase = "reply_editor_open"
	ReplyPhaseReplyContentEntered          ReplyProgressPhase = "reply_content_entered"
	ReplyPhaseSubmittedPendingVerification ReplyProgressPhase = "submitted_pending_verification"
)

// 提交按钮只允许占用一个短时限。整条任务的长时限用于定位、逐字输入和
// 提交后验收，不能让一个不可交互的按钮独占剩余时间。
const replySubmitClickTimeout = 5 * time.Second

type ReplyProgressFunc func(ReplyVerificationStatus, ReplyRequestStatus, ReplyServerStatus, ReplyProgressPhase)

// ReplyVerification 只在能证明回复属于请求目标时给出 confirmed。
// uncertain 表示动作可能已经提交，调用方绝不能自动重试。
type ReplyVerification struct {
	Status                      ReplyVerificationStatus `json:"verification_status"`
	RequestStatus               ReplyRequestStatus      `json:"request_status"`
	ServerStatus                ReplyServerStatus       `json:"server_status"`
	ServerHTTPStatus            int                     `json:"server_http_status,omitempty"`
	ServerCode                  string                  `json:"server_code,omitempty"`
	RequestedCommentID          string                  `json:"requested_comment_id"`
	ObservedTargetCommentID     string                  `json:"observed_target_comment_id,omitempty"`
	ObservedThreadRootCommentID string                  `json:"observed_thread_root_comment_id,omitempty"`
	CreatedCommentID            string                  `json:"created_comment_id,omitempty"`
	ReplyButtonAttempts         int                     `json:"reply_button_attempts,omitempty"`
	Message                     string                  `json:"message"`
}

type replyTargetMeta struct {
	CommentID  string `json:"comment_id"`
	ThreadRoot string `json:"thread_root"`
	UserID     string `json:"user_id"`
	Nickname   string `json:"nickname"`
}

type replyObservation struct {
	CommentID  string `json:"comment_id"`
	ThreadRoot string `json:"thread_root"`
	UserID     string `json:"user_id"`
}

type replyRequestGateResult struct {
	RequestStatus ReplyRequestStatus
	TargetComment string
	Reason        string
	Err           error
}

type replyServerResult struct {
	Status     ReplyServerStatus
	HTTPStatus int
	Code       string
	Reason     string
	Err        error
}

type replyRequestGate struct {
	page      *rod.Page
	cancel    context.CancelFunc
	requests  <-chan replyRequestGateResult
	responses <-chan replyServerResult
	deadline  time.Time
}

// NewCommentFeedAction 创建 Feed 评论动作
func NewCommentFeedAction(page *rod.Page) *CommentFeedAction {
	return &CommentFeedAction{page: page}
}

// PostComment 发表评论到 Feed
func (f *CommentFeedAction) PostComment(ctx context.Context, feedID, xsecToken, content string) error {
	// 不使用 Context(ctx)，避免继承外部 context 的超时
	page := f.page.Timeout(commentActionTimeout(content))

	url := makeFeedDetailURL(feedID, xsecToken)
	logrus.Infof("打开 feed 详情页: %s", url)

	// 导航到详情页
	page.MustNavigate(url)
	waitFeedPageReady(page)
	humanize.Delay(ctx, humanize.AfterNavigate)

	// 检测页面是否可访问
	if err := checkPageAccessible(page); err != nil {
		return err
	}

	elem, err := page.Element("div.input-box div.content-edit span")
	if err != nil {
		logrus.Warnf("Failed to find comment input box: %v", err)
		return fmt.Errorf("未找到评论输入框，该帖子可能不支持评论或网页端不可访问: %w", err)
	}

	if err := humanize.Click(elem); err != nil {
		logrus.Warnf("Failed to click comment input box: %v", err)
		return fmt.Errorf("无法点击评论输入框: %w", err)
	}
	humanize.Delay(ctx, humanize.AfterClick)

	elem2, err := page.Element("div.input-box div.content-edit p.content-input")
	if err != nil {
		logrus.Warnf("Failed to find comment input field: %v", err)
		return fmt.Errorf("未找到评论输入区域: %w", err)
	}

	if err := humanize.Type(ctx, elem2, content); err != nil {
		logrus.Warnf("Failed to input comment content: %v", err)
		return fmt.Errorf("无法输入评论内容: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterType)

	submitButton, err := page.Element("div.bottom button.submit")
	if err != nil {
		logrus.Warnf("Failed to find submit button: %v", err)
		return fmt.Errorf("未找到提交按钮: %w", err)
	}

	if err := humanize.Click(submitButton); err != nil {
		logrus.Warnf("Failed to click submit button: %v", err)
		return fmt.Errorf("无法点击提交按钮: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterClick)

	// 就地校验：提交后评论应在评论区渲染出现；未出现则判定失败，避免假成功。
	if !waitCommentRendered(page, content, 4*time.Second) {
		logrus.Warnf("评论提交后未在评论区渲染，判定未成功: feed=%s", feedID)
		return fmt.Errorf("评论未确认成功：提交后未在评论区出现（可能账号被限制或发送失败），feed: %s", feedID)
	}

	logrus.Infof("Comment posted and verified to feed: %s", feedID)
	return nil
}

func commentActionTimeout(content string) time.Duration {
	const (
		base    = 90 * time.Second
		perRune = 200 * time.Millisecond
		maximum = 5 * time.Minute
	)
	timeout := base + time.Duration(utf8.RuneCountInString(content))*perRune
	if timeout > maximum {
		return maximum
	}
	return timeout
}

// commentRendered 就地读当前页评论区 DOM，判断指定文本的评论是否已渲染出现。
// 不重新导航、不滚动——只读已加载的 .comments-container 的可见文本。
func commentRendered(page *rod.Page, content string) bool {
	res, err := page.Eval(`(txt) => {
		const c = document.querySelector('.comments-container');
		return c ? c.innerText.includes(txt) : false;
	}`, content)
	if err != nil {
		return false
	}
	return res.Value.Bool()
}

func waitCommentRendered(page *rod.Page, content string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if commentRendered(page, content) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// ReplyToComment 回复指定评论
func (f *CommentFeedAction) ReplyToComment(ctx context.Context, feedID, xsecToken, commentID, userID, content string) (*ReplyVerification, error) {
	return f.ReplyToCommentWithProgress(ctx, feedID, xsecToken, commentID, userID, content, nil)
}

// ReplyToCommentWithProgress 在已知请求边界变化时立即回报，便于异步任务持久化。
func (f *CommentFeedAction) ReplyToCommentWithProgress(ctx context.Context, feedID, xsecToken, commentID, userID, content string, progress ReplyProgressFunc) (result *ReplyVerification, resultErr error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return nil, fmt.Errorf("缺少 comment_id：为避免同一用户有多条评论时回复错楼，已禁用 user_id 定位")
	}

	verification := &ReplyVerification{
		Status:             ReplyUncertain,
		RequestStatus:      ReplyRequestNotStarted,
		ServerStatus:       ReplyServerNotObserved,
		RequestedCommentID: commentID,
		Message:            "尚未提交",
	}
	defer func() {
		if result != nil && progress != nil {
			progress(result.Status, result.RequestStatus, result.ServerStatus, "")
		}
	}()

	// 增加超时时间，因为需要滚动查找评论
	// 注意：不使用 Context(ctx)，避免继承外部 context 的超时
	page := f.page.Timeout(5 * time.Minute)
	url := makeFeedDetailURL(feedID, xsecToken)
	reportReplyProgress(progress, verification, ReplyPhaseLocatingComment)
	logrus.Infof("打开 feed 详情页进行回复: %s", url)

	// 导航到详情页
	page.MustNavigate(url)
	waitFeedPageReady(page)
	humanize.Delay(ctx, humanize.AfterNavigate)

	// 检测页面是否可访问
	if err := checkPageAccessible(page); err != nil {
		return stopReplyBeforeSubmit(verification, "页面不可访问，已在定位前中止", err)
	}

	time.Sleep(2 * time.Second)

	// 使用 Go 实现的查找逻辑
	commentEl, err := findCommentElement(ctx, page, commentID)
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法找到目标评论，已在提交前中止", err)
	}

	target, err := inspectReplyTarget(page, commentEl, feedID, commentID)
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法确认目标评论身份，已在提交前中止", err)
	}
	verification.ObservedThreadRootCommentID = target.ThreadRoot
	if userID != "" && target.UserID != "" && target.UserID != userID {
		return stopReplyBeforeSubmit(verification, "comment_id 对应用户与 user_id 不一致，已在提交前中止", nil)
	}
	currentUserID, err := currentUserID(page)
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法确认当前登录用户身份，已在提交前中止", err)
	}
	reportReplyProgress(progress, verification, ReplyPhaseTargetLocated)

	// 滚动到评论位置
	logrus.Info("滚动到评论位置...")
	if err := commentEl.ScrollIntoView(); err != nil {
		return stopReplyBeforeSubmit(verification, "滚动到目标评论失败，已在提交前中止", err)
	}
	humanize.Delay(ctx, humanize.BetweenScroll)

	logrus.Info("准备点击回复按钮")

	// 回复按钮会在评论区懒渲染时短暂脱离或不可交互。首次失败后只重新
	// 精确定位同一 comment_id 并再点一次；此时尚未触发提交，安全保持 not_started。
	buttonAttempts, err := clickReplyButtonWithRetry(page, commentEl, commentID)
	verification.ReplyButtonAttempts = buttonAttempts
	if err != nil {
		return stopReplyBeforeSubmit(verification, "点击回复按钮失败，已在提交前中止", err)
	}

	humanize.Delay(ctx, humanize.AfterClick)

	// 查找回复输入框
	inputEl, err := page.Element("div.input-box div.content-edit p.content-input")
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法找到回复输入框，已在提交前中止", err)
	}
	reportReplyProgress(progress, verification, ReplyPhaseReplyEditorOpen)
	before, err := snapshotReplyObservations(page, content)
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法建立回复前验收快照，已在提交前中止", err)
	}

	// 输入内容
	if err := humanize.Type(ctx, inputEl, content); err != nil {
		return stopReplyBeforeSubmit(verification, "输入回复内容失败，未点击提交", err)
	}

	humanize.Delay(ctx, humanize.AfterType)
	reportReplyProgress(progress, verification, ReplyPhaseReplyContentEntered)

	// 查找并点击提交按钮
	submitBtn, err := page.Element("div.bottom button.submit")
	if err != nil {
		return stopReplyBeforeSubmit(verification, "无法找到提交按钮，未点击提交", err)
	}
	requestGate, err := startReplyRequestGate(page, commentID, 5*time.Second)
	if err != nil {
		verification.Status = ReplyBlocked
		verification.Message = "回复请求拦截器未能启动；未点击提交，确定没有发送"
		return verification, fmt.Errorf("回复请求拦截器启动失败，已在提交前中止: %w", err)
	}
	defer requestGate.Close()

	if err := humanize.Click(submitBtn.Timeout(replySubmitClickTimeout)); err != nil {
		requestGate.Abort()
		verification.Status = ReplyUncertain
		verification.RequestStatus = ReplyRequestNotObserved
		verification.Message = "点击提交按钮时报错并已立即关闭页面；无法证明点击事件是否已触发请求，请按可能已发送处理并先核实，绝对不要重发"
		return verification, fmt.Errorf("点击提交按钮失败且请求状态不确定，请勿重发: %w", err)
	}

	humanize.Delay(ctx, humanize.AfterClick)

	gateResult, observed := waitReplyRequestGate(requestGate.requests, time.Until(requestGate.deadline))
	if !observed {
		requestGate.Abort()
		verification.Status = ReplyUncertain
		verification.RequestStatus = ReplyRequestNotObserved
		verification.Message = "提交后未在时限内观察到受控评论请求；拦截器没有主动放行任何匹配请求，但无法排除页面改走其他通道，请按可能已发送处理并先核实，绝对不要重发"
		return verification, fmt.Errorf("回复请求拦截超时：未观察到受控请求，状态不确定，请勿重发")
	}
	verification.RequestStatus = gateResult.RequestStatus
	verification.ObservedTargetCommentID = gateResult.TargetComment
	if gateResult.RequestStatus == ReplyRequestBlocked {
		reportReplyProgress(progress, verification, "")
		verification.Status = ReplyBlocked
		verification.Message = "回复请求已在浏览器网络层阻断，确定没有发送：" + gateResult.Reason
		return verification, fmt.Errorf("回复请求已阻断，确定未发送: %s", gateResult.Reason)
	}
	if gateResult.RequestStatus != ReplyRequestAllowed || gateResult.Err != nil {
		reportReplyProgress(progress, verification, "")
		verification.Status = ReplyUncertain
		verification.Message = "无法确认评论请求是否被安全放行；请按可能已发送处理并先核实，绝对不要重发"
		return verification, fmt.Errorf("回复请求拦截状态不确定，请勿重发: %v", gateResult.Err)
	}
	reportReplyProgress(progress, verification, ReplyPhaseSubmittedPendingVerification)

	serverResult, responseObserved := waitReplyServerGate(requestGate.responses, 10*time.Second)
	if responseObserved {
		verification.ServerStatus = serverResult.Status
		verification.ServerHTTPStatus = serverResult.HTTPStatus
		verification.ServerCode = serverResult.Code
		reportReplyProgress(progress, verification, "")
	}
	if responseObserved && serverResult.Status == ReplyServerRejected {
		verification.Status = ReplyRejected
		verification.Message = "回复请求已发送到正确目标，但小红书服务器明确拒绝，确定没有发布成功"
		return verification, fmt.Errorf("回复被小红书服务器拒绝: %s", serverResult.Reason)
	}

	observation, status := waitReplyObservation(page, feedID, content, before, target.ThreadRoot, currentUserID, replyVerificationTimeout(content))
	if observation != nil {
		verification.CreatedCommentID = observation.CommentID
		verification.ObservedThreadRootCommentID = observation.ThreadRoot
	}
	if status == ReplyWrongTarget {
		verification.Status = ReplyWrongTarget
		verification.Message = "已看到新回复，但它出现在其他评论楼层"
		return verification, fmt.Errorf("回复已出现但挂在错误楼层，请勿重试")
	}
	if status != ReplyConfirmed {
		verification.Status = ReplyUncertain
		verification.Message = "提交动作可能已发生，但未能同时确认提交目标和目标楼层内的新回复；请稍后用 list_notifications 或 get_feed_detail 核实，看到自己的回复即按成功处理，绝对不要重发"
		return verification, fmt.Errorf("回复状态不确定：可能已经发送；请稍后核实，绝对不要重发")
	}

	verification.Status = ReplyConfirmed
	verification.RequestStatus = ReplyRequestAllowed
	verification.Message = "已确认提交请求指向目标 comment_id，且目标楼层内出现新的本人回复"
	logrus.Infof("回复评论成功并完成目标楼层验收")
	return verification, nil
}

func reportReplyProgress(progress ReplyProgressFunc, verification *ReplyVerification, phase ReplyProgressPhase) {
	if progress != nil {
		progress(verification.Status, verification.RequestStatus, verification.ServerStatus, phase)
	}
}

func stopReplyBeforeSubmit(verification *ReplyVerification, message string, cause error) (*ReplyVerification, error) {
	verification.Status = ReplyBlocked
	verification.RequestStatus = ReplyRequestNotStarted
	verification.Message = message + "；未点击提交，确定没有发送"
	if cause != nil {
		return verification, fmt.Errorf("%s: %w", message, cause)
	}
	return verification, fmt.Errorf("%s", message)
}

func inspectReplyTarget(page *rod.Page, el *rod.Element, feedID, commentID string) (*replyTargetMeta, error) {
	detail, err := NewFeedDetailAction(page).extractFeedDetail(page, feedID)
	if err != nil {
		return nil, fmt.Errorf("无法读取详情评论树: %w", err)
	}
	target, err := resolveReplyTargetFromCommentTree(detail.Comments.List, commentID)
	if err != nil {
		return nil, err
	}

	res, err := el.Eval(`(commentID) => {
		const ownUsers = Array.from(this.querySelectorAll('[data-user-id]'))
			.filter(node => node.closest('.comment-item') === this);
		const user = ownUsers[0] || null;
		return JSON.stringify({
			comment_id: (this.id || '').replace(/^comment-/, ''),
			user_id: user ? (user.getAttribute('data-user-id') || '') : '',
			nickname: user ? (user.textContent || '').trim() : ''
		});
	}`, commentID)
	if err != nil {
		return nil, err
	}
	var dom replyTargetMeta
	if err := json.Unmarshal([]byte(res.Value.String()), &dom); err != nil {
		return nil, err
	}
	if dom.CommentID != commentID {
		return nil, fmt.Errorf("期望 %s，DOM 实际为 %s", commentID, dom.CommentID)
	}
	if target.UserID != "" && dom.UserID != "" && target.UserID != dom.UserID {
		return nil, fmt.Errorf("详情评论树与 DOM 的评论作者不一致")
	}
	if target.UserID == "" {
		target.UserID = dom.UserID
	}
	if target.Nickname == "" {
		target.Nickname = dom.Nickname
	}
	return target, nil
}

// resolveReplyTargetFromCommentTree 只使用详情接口返回的评论树确定目标层级和主楼。
// 顶层评论的主楼是它自己；任意深度的楼中楼都归属于最外层评论。
// 找不到或同一 ID 出现在多个位置时必须中止，不能退回 DOM 猜测或 user_id 定位。
func resolveReplyTargetFromCommentTree(comments []Comment, commentID string) (*replyTargetMeta, error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return nil, fmt.Errorf("详情评论树定位缺少 comment_id")
	}

	var matches []replyTargetMeta
	for _, root := range comments {
		collectReplyTreeMatches(root, root.ID, commentID, &matches)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("详情评论树中未找到目标 comment_id: %s", commentID)
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("详情评论树中目标 comment_id 出现 %d 次，归属不唯一: %s", len(matches), commentID)
	}
	return &matches[0], nil
}

func collectReplyTreeMatches(comment Comment, threadRoot, targetID string, matches *[]replyTargetMeta) {
	if comment.ID == targetID {
		nickname := comment.UserInfo.Nickname
		if nickname == "" {
			nickname = comment.UserInfo.NickName
		}
		*matches = append(*matches, replyTargetMeta{
			CommentID:  comment.ID,
			ThreadRoot: threadRoot,
			UserID:     comment.UserInfo.UserID,
			Nickname:   nickname,
		})
	}
	for _, child := range comment.SubComments {
		collectReplyTreeMatches(child, threadRoot, targetID, matches)
	}
}

func exactReplyButton(commentEl *rod.Element, commentID string) (*rod.Element, error) {
	buttons, err := commentEl.Elements(".right .interactions .reply")
	if err != nil {
		return nil, err
	}
	for _, button := range buttons {
		res, err := button.Eval(`(commentID) => {
			const owner = this.closest('.comment-item');
			return !!owner && owner.id === 'comment-' + commentID;
		}`, commentID)
		if err == nil && res.Value.Bool() {
			return button, nil
		}
	}
	return nil, fmt.Errorf("目标评论范围内没有唯一回复按钮")
}

func clickReplyButtonWithRetry(page *rod.Page, initialComment *rod.Element, commentID string) (int, error) {
	click := func(commentEl *rod.Element) error {
		replyBtn, err := exactReplyButton(commentEl, commentID)
		if err != nil {
			return err
		}
		return humanize.Click(replyBtn)
	}

	return runReplyButtonClickWithRetry(
		func() error { return click(initialComment) },
		func() error {
			freshComment := lookupComment(page, commentID)
			if freshComment == nil {
				return fmt.Errorf("重试时无法重新定位目标 comment_id: %s", commentID)
			}
			if err := freshComment.ScrollIntoView(); err != nil {
				return fmt.Errorf("重试时滚动到目标评论失败: %w", err)
			}
			time.Sleep(250 * time.Millisecond)
			return click(freshComment)
		},
		func() { time.Sleep(750 * time.Millisecond) },
	)
}

func runReplyButtonClickWithRetry(first, retry func() error, pause func()) (int, error) {
	firstErr := first()
	if firstErr == nil {
		return 1, nil
	}
	logrus.Warnf("首次点击回复按钮失败，将重新定位同一评论并重试一次: %v", firstErr)
	pause()
	if retryErr := retry(); retryErr != nil {
		return 2, fmt.Errorf("首次点击失败: %v；重新定位后再次点击仍失败: %w", firstErr, retryErr)
	}
	logrus.Info("重新定位同一评论后，第二次点击回复按钮成功")
	return 2, nil
}

func currentUserID(page *rod.Page) (string, error) {
	res, err := page.Eval(`() => {
		const user = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const info = user && user.userInfo && user.userInfo.value !== undefined
			? user.userInfo.value
			: (user && user.userInfo);
		return info && !info.guest ? (info.userId || info.user_id || '') : '';
	}`)
	if err != nil {
		return "", err
	}
	userID := strings.TrimSpace(res.Value.String())
	if userID == "" {
		return "", fmt.Errorf("页面状态没有当前用户 ID")
	}
	return userID, nil
}

func snapshotReplyObservations(page *rod.Page, content string) (map[string]replyObservation, error) {
	res, err := page.Eval(`(content) => JSON.stringify(
		Array.from(document.querySelectorAll('.comment-item[id^="comment-"]'))
			.map(node => {
				const copy = node.cloneNode(true);
				copy.querySelectorAll('.comment-item').forEach(child => child.remove());
				if (!(copy.innerText || copy.textContent || '').includes(content)) return null;
				const author = Array.from(node.querySelectorAll('[data-user-id]'))
					.find(item => item.closest('.comment-item') === node);
				return {
					comment_id: (node.id || '').replace(/^comment-/, ''),
					user_id: author ? (author.getAttribute('data-user-id') || '') : ''
				};
			})
			.filter(Boolean)
	)`, content)
	if err != nil {
		return nil, err
	}
	var items []replyObservation
	if err := json.Unmarshal([]byte(res.Value.String()), &items); err != nil {
		return nil, err
	}
	out := make(map[string]replyObservation, len(items))
	for _, item := range items {
		if item.CommentID != "" {
			out[item.CommentID] = item
		}
	}
	return out, nil
}

func waitReplyObservation(page *rod.Page, feedID, content string, before map[string]replyObservation, expectedRoot, currentUserID string, timeout time.Duration) (*replyObservation, ReplyVerificationStatus) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		after, err := snapshotReplyObservations(page, content)
		if err == nil {
			attachReplyObservationRoots(page, feedID, before, after)
			if item, status := classifyReplyObservation(before, after, expectedRoot, currentUserID); item != nil {
				return item, status
			}
		}
		time.Sleep(350 * time.Millisecond)
	}
	return nil, ReplyUncertain
}

func replyVerificationTimeout(content string) time.Duration {
	const (
		base    = 12 * time.Second
		perRune = 50 * time.Millisecond
		maximum = 45 * time.Second
	)
	timeout := base + time.Duration(utf8.RuneCountInString(content))*perRune
	if timeout > maximum {
		return maximum
	}
	return timeout
}

// attachReplyObservationRoots 用提交后更新的详情评论树给新出现的候选回复补主楼 ID。
// 解析失败时保持空值，最终只会得到 uncertain，不会误报 confirmed。
func attachReplyObservationRoots(page *rod.Page, feedID string, before, after map[string]replyObservation) {
	detail, err := NewFeedDetailAction(page).extractFeedDetail(page, feedID)
	if err != nil {
		return
	}
	for id, item := range after {
		if _, existed := before[id]; existed {
			continue
		}
		target, err := resolveReplyTargetFromCommentTree(detail.Comments.List, id)
		if err != nil {
			continue
		}
		item.ThreadRoot = target.ThreadRoot
		after[id] = item
	}
}

func classifyReplyObservation(before, after map[string]replyObservation, expectedRoot, currentUserID string) (*replyObservation, ReplyVerificationStatus) {
	var wrong *replyObservation
	for id, item := range after {
		if _, existed := before[id]; existed {
			continue
		}
		if item.UserID == "" || item.UserID != currentUserID {
			continue
		}
		if item.ThreadRoot == "" {
			continue
		}
		candidate := item
		if item.ThreadRoot == expectedRoot {
			return &candidate, ReplyConfirmed
		}
		if wrong == nil {
			wrong = &candidate
		}
	}
	if wrong != nil {
		return wrong, ReplyWrongTarget
	}
	return nil, ReplyUncertain
}

func startReplyRequestGate(page *rod.Page, expectedCommentID string, timeout time.Duration) (*replyRequestGate, error) {
	patterns := []*proto.FetchRequestPattern{
		{URLPattern: "*", ResourceType: proto.NetworkResourceTypeXHR, RequestStage: proto.FetchRequestStageRequest},
		{URLPattern: "*", ResourceType: proto.NetworkResourceTypeFetch, RequestStage: proto.FetchRequestStageRequest},
		{URLPattern: "*", ResourceType: proto.NetworkResourceTypeXHR, RequestStage: proto.FetchRequestStageResponse},
		{URLPattern: "*", ResourceType: proto.NetworkResourceTypeFetch, RequestStage: proto.FetchRequestStageResponse},
	}
	if err := (proto.FetchEnable{Patterns: patterns}).Call(page); err != nil {
		return nil, err
	}

	gateCtx, cancel := context.WithCancel(page.GetContext())
	requests := make(chan replyRequestGateResult, 1)
	responses := make(chan replyServerResult, 1)
	deadline := time.Now().Add(timeout)
	var targetRequestID proto.FetchRequestID
	wait := page.Context(gateCtx).EachEvent(func(event *proto.FetchRequestPaused) bool {
		if replyEventAtResponseStage(event) {
			if targetRequestID != "" && event.RequestID == targetRequestID {
				responses <- handlePausedReplyResponse(page, event)
				return true
			}
			if err := continueUnrelatedResponse(page, event); err != nil {
				responses <- replyServerResult{Status: ReplyServerUnknown, Err: err, Reason: err.Error()}
				return true
			}
			return false
		}
		if !isPotentialReplyPost(event) {
			if err := continueUnrelatedRequest(page, event); err != nil {
				requests <- replyRequestGateResult{RequestStatus: ReplyRequestNotObserved, Err: err}
				return true
			}
			return false
		}
		result := handlePausedReplyRequest(page, event, expectedCommentID, deadline)
		requests <- result
		if result.RequestStatus != ReplyRequestAllowed {
			return true
		}
		targetRequestID = event.RequestID
		return false
	})
	go wait()

	return &replyRequestGate{page: page, cancel: cancel, requests: requests, responses: responses, deadline: deadline}, nil
}

func replyEventAtResponseStage(event *proto.FetchRequestPaused) bool {
	return event != nil && (event.ResponseStatusCode != nil || event.ResponseErrorReason != "")
}

func continueUnrelatedResponse(client proto.Client, event *proto.FetchRequestPaused) error {
	if event == nil {
		return fmt.Errorf("无关响应事件为空")
	}
	if err := (proto.FetchContinueResponse{RequestID: event.RequestID}).Call(client); err != nil {
		return fmt.Errorf("放行无关响应失败: %w", err)
	}
	return nil
}

func handlePausedReplyResponse(client proto.Client, event *proto.FetchRequestPaused) replyServerResult {
	result := replyServerResult{Status: ReplyServerUnknown}
	if event == nil || event.Request == nil {
		result.Reason = "响应事件缺少请求数据"
		result.Err = errors.New(result.Reason)
		return result
	}
	if event.ResponseStatusCode != nil {
		result.HTTPStatus = *event.ResponseStatusCode
	}
	if event.ResponseErrorReason != "" {
		result.Reason = "评论请求发生网络错误"
		result.Err = fmt.Errorf("network error: %s", event.ResponseErrorReason)
		return result
	}
	bodyResult, bodyErr := (proto.FetchGetResponseBody{RequestID: event.RequestID}).Call(client)
	if bodyErr != nil {
		result.Reason = "无法读取评论响应"
		result.Err = bodyErr
		_ = (proto.FetchContinueResponse{RequestID: event.RequestID}).Call(client)
		return result
	}
	body := []byte(bodyResult.Body)
	if bodyResult.Base64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(bodyResult.Body)
		if err != nil {
			result.Reason = "无法解码评论响应"
			result.Err = err
			_ = (proto.FetchContinueResponse{RequestID: event.RequestID}).Call(client)
			return result
		}
		body = decoded
	}
	classified := classifyCommentPostResponse(result.HTTPStatus, body)
	if err := (proto.FetchContinueResponse{RequestID: event.RequestID}).Call(client); err != nil {
		classified.Status = ReplyServerUnknown
		classified.Reason = "读取后无法继续评论响应"
		classified.Err = err
	}
	return classified
}

func classifyCommentPostResponse(httpStatus int, body []byte) replyServerResult {
	result := replyServerResult{Status: ReplyServerUnknown, HTTPStatus: httpStatus}
	if httpStatus < 200 || httpStatus >= 300 {
		result.Status = ReplyServerRejected
		result.Reason = fmt.Sprintf("HTTP %d", httpStatus)
		return result
	}
	var payload struct {
		Success *bool           `json:"success"`
		Code    json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		result.Reason = "HTTP 成功但响应格式无法识别"
		return result
	}
	if payload.Success != nil {
		if *payload.Success {
			result.Status = ReplyServerAccepted
			result.Reason = "success=true"
		} else {
			result.Status = ReplyServerRejected
			result.Reason = "success=false"
		}
		return result
	}
	if len(payload.Code) == 0 || string(payload.Code) == "null" {
		result.Reason = "HTTP 成功但响应缺少 success/code"
		return result
	}
	var number json.Number
	if err := json.Unmarshal(payload.Code, &number); err == nil {
		result.Code = number.String()
		if result.Code == "0" {
			result.Status = ReplyServerAccepted
			result.Reason = "code=0"
		} else {
			result.Status = ReplyServerRejected
			result.Reason = "code=" + result.Code
		}
		return result
	}
	var code string
	if err := json.Unmarshal(payload.Code, &code); err == nil {
		result.Code = strings.TrimSpace(code)
		normalized := strings.ToLower(result.Code)
		if normalized == "0" || normalized == "ok" || normalized == "success" {
			result.Status = ReplyServerAccepted
			result.Reason = "code=" + result.Code
		} else if result.Code != "" {
			result.Status = ReplyServerRejected
			result.Reason = "code=" + result.Code
		}
		return result
	}
	result.Reason = "HTTP 成功但 code 无法识别"
	return result
}

// isPotentialReplyPost 在短暂提交窗口内识别可能的评论写请求。
// 即使接口版本或路径改变，只要仍是 POST 且 URL 含 comment/post，或请求体仍携带
// target_comment_id，也会进入严格校验而不会静默穿过拦截器。
func isPotentialReplyPost(event *proto.FetchRequestPaused) bool {
	if event == nil || event.Request == nil || event.Request.Method != "POST" {
		return false
	}
	urlLooksLikeCommentPost := strings.Contains(event.Request.URL, "comment") && strings.Contains(event.Request.URL, "post")
	bodyHasReplyTarget := strings.Contains(event.Request.PostData, `"target_comment_id"`)
	return urlLooksLikeCommentPost || bodyHasReplyTarget
}

func continueUnrelatedRequest(client proto.Client, event *proto.FetchRequestPaused) error {
	if event == nil {
		return fmt.Errorf("无关请求事件为空")
	}
	if err := (proto.FetchContinueRequest{RequestID: event.RequestID}).Call(client); err != nil {
		return fmt.Errorf("放行无关请求失败: %w", err)
	}
	return nil
}

func (g *replyRequestGate) Close() {
	if g == nil {
		return
	}
	g.cancel()
	_ = (proto.FetchDisable{}).Call(g.page)
}

// Abort 在超时时直接关闭页面，让所有尚未放行或仍在飞行中的请求随页面终止。
// 不调用 Fetch.disable，避免浏览器在解除拦截时自行放行暂停请求。
func (g *replyRequestGate) Abort() {
	if g == nil {
		return
	}
	g.cancel()
	_ = g.page.Close()
}

func handlePausedReplyRequest(client proto.Client, event *proto.FetchRequestPaused, expectedCommentID string, deadline time.Time) replyRequestGateResult {
	if event == nil || event.Request == nil {
		return replyRequestGateResult{RequestStatus: ReplyRequestNotObserved, Err: fmt.Errorf("拦截事件缺少请求数据")}
	}

	target, allowed, reason := validateReplyPostRequest(expectedCommentID, event.Request.Method, event.Request.URL, event.Request.PostData)
	if time.Now().After(deadline) {
		allowed = false
		reason = "评论请求到达时已超过拦截截止时间"
	}
	if !allowed {
		err := (proto.FetchFailRequest{
			RequestID:   event.RequestID,
			ErrorReason: proto.NetworkErrorReasonBlockedByClient,
		}).Call(client)
		if err != nil {
			return replyRequestGateResult{RequestStatus: ReplyRequestNotObserved, TargetComment: target, Reason: reason, Err: fmt.Errorf("阻断请求失败: %w", err)}
		}
		return replyRequestGateResult{RequestStatus: ReplyRequestBlocked, TargetComment: target, Reason: reason}
	}

	if time.Now().After(deadline) {
		err := (proto.FetchFailRequest{
			RequestID:   event.RequestID,
			ErrorReason: proto.NetworkErrorReasonBlockedByClient,
		}).Call(client)
		if err == nil {
			return replyRequestGateResult{RequestStatus: ReplyRequestBlocked, TargetComment: target, Reason: "放行前超过拦截截止时间"}
		}
		return replyRequestGateResult{RequestStatus: ReplyRequestNotObserved, TargetComment: target, Err: fmt.Errorf("超时请求阻断失败: %w", err)}
	}
	if err := (proto.FetchContinueRequest{RequestID: event.RequestID}).Call(client); err != nil {
		blockErr := (proto.FetchFailRequest{
			RequestID:   event.RequestID,
			ErrorReason: proto.NetworkErrorReasonBlockedByClient,
		}).Call(client)
		if blockErr == nil {
			return replyRequestGateResult{RequestStatus: ReplyRequestBlocked, TargetComment: target, Reason: "目标正确，但请求放行失败后已阻断"}
		}
		return replyRequestGateResult{RequestStatus: ReplyRequestNotObserved, TargetComment: target, Err: fmt.Errorf("放行失败且无法确认阻断: %v; %w", err, blockErr)}
	}
	return replyRequestGateResult{RequestStatus: ReplyRequestAllowed, TargetComment: target}
}

func validateReplyPostRequest(expectedCommentID, method, requestURL, postData string) (target string, allowed bool, reason string) {
	if method != "POST" {
		return "", false, "拦截到的评论请求方法不是 POST"
	}
	if !strings.Contains(requestURL, "/api/sns/web/v1/comment/post") {
		return "", false, "拦截到的请求不是预期评论接口"
	}
	target = parseCommentPostTarget(postData)
	if target == "" {
		return "", false, "评论请求缺少可解析的 target_comment_id"
	}
	if target != expectedCommentID {
		return target, false, fmt.Sprintf("请求目标 %s 与期望 %s 不一致", target, expectedCommentID)
	}
	return target, true, ""
}

func parseCommentPostTarget(postData string) string {
	var payload struct {
		TargetCommentID string `json:"target_comment_id"`
	}
	if err := json.Unmarshal([]byte(postData), &payload); err != nil {
		return ""
	}
	return payload.TargetCommentID
}

func waitReplyRequestGate(results <-chan replyRequestGateResult, timeout time.Duration) (replyRequestGateResult, bool) {
	select {
	case result := <-results:
		return result, true
	case <-time.After(timeout):
		return replyRequestGateResult{}, false
	}
}

func waitReplyServerGate(results <-chan replyServerResult, timeout time.Duration) (replyServerResult, bool) {
	select {
	case result := <-results:
		return result, true
	case <-time.After(timeout):
		return replyServerResult{Status: ReplyServerNotObserved}, false
	}
}

// findCommentElement 滚动并展开楼中楼，只接受 commentID 精确命中。
// 二级评论折叠在「展开 N 条回复」后面，未展开时不在 DOM 里，因此查找过程中要展开。
func findCommentElement(ctx context.Context, page *rod.Page, commentID string) (*rod.Element, error) {
	logrus.Infof("开始按 commentID 精确查找评论: %s", commentID)

	scrollToCommentsArea(page)
	humanize.Delay(ctx, humanize.BetweenScroll)

	lastCommentCount := 0
	stagnantChecks := 0
	expandRounds := 0
	deadline := time.Now().Add(maxSearchDuration)

	// attempt 只数下滚，展开不计入，由 deadline 收口。
	for attempt := 0; attempt < maxSearchScrolls; {
		if time.Now().After(deadline) {
			logrus.Warnf("查找超过 %s，停止", maxSearchDuration)
			return nil, fmt.Errorf("评论区过大，%s 内未找到目标 comment_id: %s", maxSearchDuration, commentID)
		}

		// 1. 先查已渲染的评论
		if el := lookupComment(page, commentID); el != nil {
			logrus.Infof("✓ 找到目标评论（下滚 %d 次）", attempt)
			return el, nil
		}

		// 2. 展开视野里的楼中楼后再查一次
		if expandRounds < maxExpandRounds {
			if expanded := expandNearbyReplies(ctx, page); expanded > 0 {
				expandRounds++
				humanize.Delay(ctx, humanize.Reading)

				if el := lookupComment(page, commentID); el != nil {
					logrus.Infof("✓ 展开楼中楼后找到目标评论（下滚 %d 次）", attempt)
					return el, nil
				}

				continue
			}
		}
		expandRounds = 0
		attempt++

		// 3. .end-container 可能预先存在于 DOM；只有评论滚动容器真的抵达
		// 底部时才能停止，否则会在第一屏误判到底，永远看不到后续楼层。
		if checkEndContainer(page) && commentScrollerAtBottom(page) {
			logrus.Info("已到达评论底部，未找到目标评论")
			break
		}

		// 4. 评论数停滞说明加载不动了
		currentCount := getCommentCount(page)
		if currentCount != lastCommentCount {
			lastCommentCount = currentCount
			stagnantChecks = 0
		} else {
			stagnantChecks++
			if stagnantChecks >= 10 {
				logrus.Info("评论数量停滞，停止查找")
				break
			}
		}

		// 5. 滚到最后一条评论再继续下滚，触发懒加载
		if currentCount > 0 {
			if elements, err := page.Timeout(2 * time.Second).Elements(".comment-item"); err == nil && len(elements) > 0 {
				if err := elements[len(elements)-1].ScrollIntoView(); err != nil {
					logrus.Debugf("滚动到最后一条评论失败: %v", err)
				}
			}
			humanize.Delay(ctx, humanize.BetweenScroll)
		}

		humanScroll(ctx, page, "normal", false, 1)
		humanize.Delay(ctx, humanize.BetweenScroll)
	}

	return nil, fmt.Errorf("未找到目标 comment_id: %s", commentID)
}

func commentScrollerAtBottom(page *rod.Page) bool {
	res, err := page.Eval(`(selectors) => {
		for (const selector of selectors) {
			const el = document.querySelector(selector);
			if (!el || el.scrollHeight <= el.clientHeight) continue;
			return el.scrollTop + el.clientHeight >= el.scrollHeight - 8;
		}
		const doc = document.scrollingElement || document.documentElement;
		return doc.scrollTop + window.innerHeight >= doc.scrollHeight - 8;
	}`, commentScrollerSelectors)
	return err == nil && res.Value.Bool()
}

// lookupComment 在当前已渲染的评论里查找目标，找不到返回 nil。
func lookupComment(page *rod.Page, commentID string) *rod.Element {
	if commentID != "" {
		if el, err := page.Timeout(2 * time.Second).Element(fmt.Sprintf("#comment-%s", commentID)); err == nil && el != nil {
			return el
		}
	}
	return nil
}
