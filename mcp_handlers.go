// Modified from upstream: adds async task handling and bounded MCP responses.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// MCP 工具处理函数

// parseVisibility 从 MCP 参数中解析可见范围
func parseVisibility(args map[string]interface{}) string {
	v, ok := args["visibility"]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// handleCheckLoginStatus 处理检查登录状态
func (s *AppServer) handleCheckLoginStatus(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 检查登录状态")

	status, err := s.xiaohongshuService.CheckLoginStatus(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "检查登录状态失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	var resultText string
	if status.IsLoggedIn {
		resultText = fmt.Sprintf("✅ 已登录\n用户名: %s\n\n你可以使用其他功能了。", status.Username)
	} else {
		resultText = "❌ 未登录\n\n请使用 get_login_qrcode 工具获取二维码进行登录。"
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handleGetLoginQrcode 处理获取登录二维码请求。
// 返回二维码图片的 Base64 编码和超时时间，供前端展示扫码登录。
func (s *AppServer) handleGetLoginQrcode(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取登录扫码图片")

	result, err := s.xiaohongshuService.GetLoginQrcode(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取登录扫码图片失败: " + err.Error()}},
			IsError: true,
		}
	}

	if result.IsLoggedIn {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "你当前已处于登录状态"}},
		}
	}

	now := time.Now()
	deadline := func() string {
		d, err := time.ParseDuration(result.Timeout)
		if err != nil {
			return now.Format("2006-01-02 15:04:05")
		}
		return now.Add(d).Format("2006-01-02 15:04:05")
	}()

	// 已登录：文本 + 图片
	contents := []MCPContent{
		{Type: "text", Text: "请用小红书 App 在 " + deadline + " 前扫码登录 👇"},
		{
			Type:     "image",
			MimeType: "image/png",
			Data:     strings.TrimPrefix(result.Img, "data:image/png;base64,"),
		},
	}
	return &MCPToolResult{Content: contents}
}

// handleDeleteCookies 处理删除 cookies 请求，用于登录重置
func (s *AppServer) handleDeleteCookies(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 删除 cookies，重置登录状态")

	err := s.xiaohongshuService.DeleteCookies(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "删除 cookies 失败: " + err.Error()}},
			IsError: true,
		}
	}

	cookiePath := cookies.GetCookiesFilePath()
	resultText := fmt.Sprintf("Cookies 已成功删除，登录状态已重置。\n\n删除的文件路径: %s\n\n下次操作时，需要重新登录。", cookiePath)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

// handlePublishContent 处理发布内容
func (s *AppServer) handlePublishContent(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	_ = ctx
	logrus.Info("MCP: 提交异步发布任务")

	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	imagePathsInterface, _ := args["images"].([]interface{})
	tagsInterface, _ := args["tags"].([]interface{})
	productsInterface, _ := args["products"].([]interface{})

	var imagePaths []string
	for _, path := range imagePathsInterface {
		if pathStr, ok := path.(string); ok {
			imagePaths = append(imagePaths, pathStr)
		}
	}

	var tags []string
	for _, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags = append(tags, tagStr)
		}
	}

	var products []string
	for _, p := range productsInterface {
		if pStr, ok := p.(string); ok {
			products = append(products, pStr)
		}
	}

	scheduleAt, _ := args["schedule_at"].(string)
	visibility := parseVisibility(args)

	isOriginal, _ := args["is_original"].(bool)
	idempotencyKey, _ := args["idempotency_key"].(string)

	logrus.Infof("MCP: 异步发布参数 - 图片数量: %d, 标签数量: %d, 是否定时: %v, 原创: %v, 是否指定可见范围: %v, 商品数量: %d, 调用方幂等键: %v",
		len(imagePaths), len(tags), scheduleAt != "", isOriginal, visibility != "", len(products), idempotencyKey != "")

	req := &PublishRequest{
		Title:      title,
		Content:    content,
		Images:     imagePaths,
		Tags:       tags,
		ScheduleAt: scheduleAt,
		IsOriginal: isOriginal,
		Visibility: visibility,
		Products:   products,
	}

	result, err := s.publishTasks.Submit(req, idempotencyKey)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布任务提交失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "发布任务已受理，但结果编码失败: " + err.Error()}},
			IsError: true,
		}
	}
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(encoded),
		}},
	}
}

func (s *AppServer) handleGetPublishStatus(ctx context.Context, taskID string) *MCPToolResult {
	_ = ctx
	if strings.HasPrefix(strings.TrimSpace(taskID), "xhs_comment_") {
		result, ok := s.commentTasks.Get(taskID)
		if !ok {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "未找到顶层评论任务"}}, IsError: true}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "顶层评论任务状态编码失败: " + err.Error()}}, IsError: true}
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}}
	}
	if strings.HasPrefix(strings.TrimSpace(taskID), "xhs_reply_") {
		result, ok := s.replyTasks.Get(taskID)
		if !ok {
			return &MCPToolResult{
				Content: []MCPContent{{Type: "text", Text: "未找到回复任务"}},
				IsError: true,
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return &MCPToolResult{
				Content: []MCPContent{{Type: "text", Text: "回复任务状态编码失败: " + err.Error()}},
				IsError: true,
			}
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}}
	}
	result, ok := s.publishTasks.Get(taskID)
	if !ok {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "未找到发布任务"}},
			IsError: true,
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "发布任务状态编码失败: " + err.Error()}},
			IsError: true,
		}
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}}
}

// handlePublishVideo 处理发布视频内容（仅本地单个视频文件）
func (s *AppServer) handlePublishVideo(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 发布视频内容（本地）")

	title, _ := args["title"].(string)
	content, _ := args["content"].(string)
	videoPath, _ := args["video"].(string)
	tagsInterface, _ := args["tags"].([]interface{})
	productsInterface, _ := args["products"].([]interface{})

	var tags []string
	for _, tag := range tagsInterface {
		if tagStr, ok := tag.(string); ok {
			tags = append(tags, tagStr)
		}
	}

	var products []string
	for _, p := range productsInterface {
		if pStr, ok := p.(string); ok {
			products = append(products, pStr)
		}
	}

	if videoPath == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布失败: 缺少本地视频文件路径",
			}},
			IsError: true,
		}
	}

	scheduleAt, _ := args["schedule_at"].(string)
	visibility := parseVisibility(args)

	logrus.Infof("MCP: 发布视频 - 标题: %s, 标签数量: %d, 定时: %s, visibility: %s, 商品: %v", title, len(tags), scheduleAt, visibility, products)

	req := &PublishVideoRequest{
		Title:      title,
		Content:    content,
		Video:      videoPath,
		Tags:       tags,
		ScheduleAt: scheduleAt,
		Visibility: visibility,
		Products:   products,
	}

	result, err := s.xiaohongshuService.PublishVideo(ctx, req)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发布失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	resultText := fmt.Sprintf("视频发布成功: %+v", result)
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: resultText,
		}},
	}
}

const (
	defaultFeedListLimit = 20
	maxFeedListLimit     = 30
)

// FeedListItem 是首页推荐的精简卡片。
// feed_id 和 xsec_token 仅用于后续调用详情、点赞、收藏等工具。
type FeedListItem struct {
	FeedID       string `json:"feed_id"`
	XsecToken    string `json:"xsec_token"`
	UserID       string `json:"user_id"`
	Author       string `json:"author"`
	Title        string `json:"title"`
	ShareCount   string `json:"share_count"`
	LikeCount    string `json:"like_count"`
	CommentCount string `json:"comment_count"`
}

type SlimFeedsListResponse struct {
	Feeds         []FeedListItem `json:"feeds"`
	TotalCount    int            `json:"total_count"`
	ReturnedCount int            `json:"returned_count"`
}

func normalizeFeedListLimit(limit int) int {
	if limit <= 0 {
		return defaultFeedListLimit
	}
	if limit > maxFeedListLimit {
		return maxFeedListLimit
	}
	return limit
}

func slimFeeds(feeds []xiaohongshu.Feed, limit int) SlimFeedsListResponse {
	limit = normalizeFeedListLimit(limit)
	if limit > len(feeds) {
		limit = len(feeds)
	}

	items := make([]FeedListItem, 0, limit)
	for _, feed := range feeds[:limit] {
		author := feed.NoteCard.User.Nickname
		if author == "" {
			author = feed.NoteCard.User.NickName
		}
		items = append(items, FeedListItem{
			FeedID:       feed.ID,
			XsecToken:    feed.XsecToken,
			UserID:       feed.NoteCard.User.UserID,
			Author:       author,
			Title:        feed.NoteCard.DisplayTitle,
			ShareCount:   feed.NoteCard.InteractInfo.SharedCount,
			LikeCount:    feed.NoteCard.InteractInfo.LikedCount,
			CommentCount: feed.NoteCard.InteractInfo.CommentCount,
		})
	}

	return SlimFeedsListResponse{
		Feeds:         items,
		TotalCount:    len(feeds),
		ReturnedCount: len(items),
	}
}

// handleListFeeds 处理获取Feeds列表
func (s *AppServer) handleListFeeds(ctx context.Context, args ListFeedsArgs) *MCPToolResult {
	logrus.Info("MCP: 获取Feeds列表")

	result, err := s.xiaohongshuService.ListFeeds(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feeds列表失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	slimResult := slimFeeds(result.Feeds, args.Limit)
	jsonData, err := json.Marshal(slimResult)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取Feeds列表成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleSearchFeeds 处理搜索Feeds
func (s *AppServer) handleSearchFeeds(ctx context.Context, args SearchFeedsArgs) *MCPToolResult {
	logrus.Info("MCP: 搜索Feeds")

	if args.Keyword == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "搜索Feeds失败: 缺少关键词参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 搜索Feeds - 关键词: %s", args.Keyword)

	filter := xiaohongshu.FilterOption{
		SortBy:      args.Filters.SortBy,
		NoteType:    args.Filters.NoteType,
		PublishTime: args.Filters.PublishTime,
		SearchScope: args.Filters.SearchScope,
		Location:    args.Filters.Location,
	}

	result, err := s.xiaohongshuService.SearchFeeds(ctx, args.Keyword, filter)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "搜索Feeds失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	slimResult := slimFeeds(result.Feeds, args.Limit)
	jsonData, err := json.Marshal(slimResult)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("搜索Feeds成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleGetFeedDetail 处理获取Feed详情
func (s *AppServer) handleGetFeedDetail(ctx context.Context, args map[string]any) *MCPToolResult {
	logrus.Info("MCP: 获取Feed详情")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	loadAll := false
	if raw, ok := args["load_all_comments"]; ok {
		switch v := raw.(type) {
		case bool:
			loadAll = v
		case string:
			if parsed, err := strconv.ParseBool(v); err == nil {
				loadAll = parsed
			}
		case float64:
			loadAll = v != 0
		}
	}

	// 解析评论配置参数，如果未提供则使用默认值
	config := xiaohongshu.DefaultCommentLoadConfig()

	if raw, ok := args["click_more_replies"]; ok {
		switch v := raw.(type) {
		case bool:
			config.ClickMoreReplies = v
		case string:
			if parsed, err := strconv.ParseBool(v); err == nil {
				config.ClickMoreReplies = parsed
			}
		}
	}

	if raw, ok := args["max_replies_threshold"]; ok {
		switch v := raw.(type) {
		case float64:
			config.MaxRepliesThreshold = int(v)
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				config.MaxRepliesThreshold = parsed
			}
		case int:
			config.MaxRepliesThreshold = v
		}
	}

	if raw, ok := args["max_comment_items"]; ok {
		switch v := raw.(type) {
		case float64:
			config.MaxCommentItems = int(v)
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				config.MaxCommentItems = parsed
			}
		case int:
			config.MaxCommentItems = v
		}
	}

	if raw, ok := args["scroll_speed"].(string); ok && raw != "" {
		config.ScrollSpeed = raw
	}

	logrus.Infof("MCP: 获取Feed详情 - Feed ID: %s, loadAllComments=%v, config=%+v", feedID, loadAll, config)

	result, err := s.xiaohongshuService.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAll, config)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取Feed详情失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取Feed详情成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleUserProfile 获取用户主页
func (s *AppServer) handleUserProfile(ctx context.Context, args map[string]any) *MCPToolResult {
	logrus.Info("MCP: 获取用户主页")

	userID, ok := args["user_id"].(string)
	if !ok || userID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: 缺少user_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	logrus.Infof("MCP: 获取用户主页 - User ID: %s", userID)

	tab, _ := args["tab"].(string)

	result, err := s.xiaohongshuService.UserProfile(ctx, userID, xsecToken, tab)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取用户主页失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	limit, _ := args["limit"].(int)
	slimResult := slimUserProfile(result, limit)
	jsonData, err := json.Marshal(slimResult)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取用户主页，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleLikeFeed 处理点赞/取消点赞
func (s *AppServer) handleLikeFeed(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少feed_id参数"}}, IsError: true}
	}
	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少xsec_token参数"}}, IsError: true}
	}
	unlike, _ := args["unlike"].(bool)

	var res *ActionResult
	var err error

	if unlike {
		res, err = s.xiaohongshuService.UnlikeFeed(ctx, feedID, xsecToken)
	} else {
		res, err = s.xiaohongshuService.LikeFeed(ctx, feedID, xsecToken)
	}

	if err != nil {
		action := "点赞"
		if unlike {
			action = "取消点赞"
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: action + "失败: " + err.Error()}}, IsError: true}
	}

	action := "点赞"
	if unlike {
		action = "取消点赞"
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("%s成功 - Feed ID: %s", action, res.FeedID)}}}
}

// handleFavoriteFeed 处理收藏/取消收藏
func (s *AppServer) handleFavoriteFeed(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少feed_id参数"}}, IsError: true}
	}
	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "操作失败: 缺少xsec_token参数"}}, IsError: true}
	}
	unfavorite, _ := args["unfavorite"].(bool)

	var res *ActionResult
	var err error

	if unfavorite {
		res, err = s.xiaohongshuService.UnfavoriteFeed(ctx, feedID, xsecToken)
	} else {
		res, err = s.xiaohongshuService.FavoriteFeed(ctx, feedID, xsecToken)
	}

	if err != nil {
		action := "收藏"
		if unfavorite {
			action = "取消收藏"
		}
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: action + "失败: " + err.Error()}}, IsError: true}
	}

	action := "收藏"
	if unfavorite {
		action = "取消收藏"
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("%s成功 - Feed ID: %s", action, res.FeedID)}}}
}

// handlePostComment 处理发表评论到Feed
func (s *AppServer) handlePostComment(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 发表评论到Feed")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	content, ok := args["content"].(string)
	if !ok || content == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "发表评论失败: 缺少content参数",
			}},
			IsError: true,
		}
	}
	idempotencyKey, _ := args["idempotency_key"].(string)
	logrus.Infof("MCP: 异步发表评论 - Feed ID: %s, 内容长度: %d, 调用方幂等键: %v", feedID, len(content), idempotencyKey != "")

	result, err := s.commentTasks.Submit(&CommentTaskRequest{
		FeedID: feedID, XsecToken: xsecToken, Content: content,
	}, idempotencyKey)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "顶层评论任务提交失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "顶层评论任务已受理，但结果编码失败: " + err.Error()}}, IsError: true}
	}
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(encoded),
		}},
	}
}

func (s *AppServer) handleGetCommentStatus(ctx context.Context, args GetCommentStatusArgs) *MCPToolResult {
	_ = ctx
	taskID := strings.TrimSpace(args.TaskID)
	feedID := strings.TrimSpace(args.FeedID)
	if taskID != "" && feedID != "" {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "task_id 与 feed_id 两种查询方式不能同时使用"}}, IsError: true}
	}
	var result interface{}
	if taskID != "" {
		view, ok := s.commentTasks.Get(taskID)
		if !ok {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "未找到顶层评论任务"}}, IsError: true}
		}
		result = view
	} else {
		if feedID == "" {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "请提供 task_id 或 feed_id"}}, IsError: true}
		}
		lookup := s.commentTasks.FindByFeed(feedID)
		if lookup.MatchCount == 0 {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "未找到该笔记对应的顶层评论任务"}}, IsError: true}
		}
		result = lookup
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "顶层评论任务状态编码失败: " + err.Error()}}, IsError: true}
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}}
}

// handleReplyComment 处理回复评论
func (s *AppServer) handleReplyComment(ctx context.Context, args map[string]interface{}) *MCPToolResult {
	logrus.Info("MCP: 回复评论")

	feedID, ok := args["feed_id"].(string)
	if !ok || feedID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少feed_id参数",
			}},
			IsError: true,
		}
	}

	xsecToken, ok := args["xsec_token"].(string)
	if !ok || xsecToken == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少xsec_token参数",
			}},
			IsError: true,
		}
	}

	commentID, _ := args["comment_id"].(string)
	userID, _ := args["user_id"].(string)
	if commentID == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少comment_id参数；为避免错楼，user_id不能用于兜底定位",
			}},
			IsError: true,
		}
	}

	content, ok := args["content"].(string)
	if !ok || content == "" {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复评论失败: 缺少content参数",
			}},
			IsError: true,
		}
	}
	idempotencyKey, _ := args["idempotency_key"].(string)

	logrus.Infof("MCP: 回复评论 - Feed ID: %s, Comment ID: %s, User ID: %s, 内容长度: %d", feedID, commentID, userID, len(content))

	result, err := s.replyTasks.Submit(&ReplyTaskRequest{
		FeedID: feedID, XsecToken: xsecToken, CommentID: commentID, UserID: userID, Content: content,
	}, idempotencyKey)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "回复任务提交失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	jsonData, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "回复任务已受理，但结果序列化失败: " + err.Error()}},
			IsError: true,
		}
	}
	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

func (s *AppServer) handleGetReplyStatus(ctx context.Context, args GetReplyStatusArgs) *MCPToolResult {
	_ = ctx
	taskID := strings.TrimSpace(args.TaskID)
	feedID := strings.TrimSpace(args.FeedID)
	commentID := strings.TrimSpace(args.CommentID)
	if taskID != "" && (feedID != "" || commentID != "") {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "task_id 与 feed_id+comment_id 两种查询方式不能同时使用"}}, IsError: true}
	}
	var result interface{}
	if taskID != "" {
		view, ok := s.replyTasks.Get(taskID)
		if !ok {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "未找到回复任务"}}, IsError: true}
		}
		result = view
	} else {
		if feedID == "" || commentID == "" {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "请提供 task_id，或同时提供 feed_id 和 comment_id"}}, IsError: true}
		}
		lookup := s.replyTasks.FindByTarget(feedID, commentID)
		if lookup.MatchCount == 0 {
			return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "未找到该笔记与评论对应的回复任务"}}, IsError: true}
		}
		result = lookup
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: "回复任务状态编码失败: " + err.Error()}}, IsError: true}
	}
	return &MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}}
}

type SlimUserBasicInfo struct {
	Nickname    string `json:"nickname"`
	Description string `json:"description"`
	Location    string `json:"location"`
	RedID       string `json:"red_id"`
}

type SlimUserProfileResponse struct {
	Profile       SlimUserBasicInfo              `json:"profile"`
	Interactions  []xiaohongshu.UserInteractions `json:"interactions"`
	Feeds         []FeedListItem                 `json:"feeds"`
	TotalCount    int                            `json:"total_count"`
	ReturnedCount int                            `json:"returned_count"`
}

func slimUserProfile(result *UserProfileResponse, limit int) SlimUserProfileResponse {
	feeds := slimFeeds(result.Feeds, limit)
	return SlimUserProfileResponse{
		Profile: SlimUserBasicInfo{
			Nickname:    result.UserBasicInfo.Nickname,
			Description: result.UserBasicInfo.Desc,
			Location:    result.UserBasicInfo.IpLocation,
			RedID:       result.UserBasicInfo.RedId,
		},
		Interactions:  result.Interactions,
		Feeds:         feeds.Feeds,
		TotalCount:    feeds.TotalCount,
		ReturnedCount: feeds.ReturnedCount,
	}
}

// handleGetMyProfile 获取当前登录用户主页
func (s *AppServer) handleGetMyProfile(ctx context.Context, tab string, limit int) *MCPToolResult {
	logrus.Infof("MCP: 获取我的主页 tab=%s", tab)

	result, err := s.xiaohongshuService.GetMyProfile(ctx, tab)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: "获取我的主页失败: " + err.Error(),
			}},
			IsError: true,
		}
	}

	slimResult := slimUserProfile(result, limit)
	jsonData, err := json.Marshal(slimResult)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("获取我的主页成功，但序列化失败: %v", err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{
			Type: "text",
			Text: string(jsonData),
		}},
	}
}

// handleGetUnreadCount 获取通知未读数
func (s *AppServer) handleGetUnreadCount(ctx context.Context) *MCPToolResult {
	logrus.Info("MCP: 获取通知未读数")

	result, err := s.xiaohongshuService.GetUnreadCount(ctx)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取未读数失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "获取未读数")
}

// handleListNotifications 获取通知列表
func (s *AppServer) handleListNotifications(ctx context.Context, tab string, limit int) *MCPToolResult {
	logrus.Infof("MCP: 获取通知列表 tab=%s limit=%d", tab, limit)

	result, err := s.xiaohongshuService.ListNotifications(ctx, tab, limit)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "获取通知列表失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "获取通知列表")
}

// handleReplyNotification 回复通知里的评论
func (s *AppServer) handleReplyNotification(ctx context.Context, commentID, content string) *MCPToolResult {
	logrus.Infof("MCP: 回复通知评论 comment=%s", commentID)

	result, err := s.xiaohongshuService.ReplyNotification(ctx, commentID, content)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "回复失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "回复")
}

// marshalMCPResult 把结果序列化成 MCP 文本内容。
func marshalMCPResult(result any, action string) *MCPToolResult {
	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("%s成功，但序列化失败: %v", action, err),
			}},
			IsError: true,
		}
	}

	return &MCPToolResult{
		Content: []MCPContent{{Type: "text", Text: string(jsonData)}},
	}
}

// handleLikeNotification 给通知里的评论点赞/取消点赞
func (s *AppServer) handleLikeNotification(ctx context.Context, commentID string, unlike bool) *MCPToolResult {
	logrus.Infof("MCP: 通知点赞 comment=%s unlike=%v", commentID, unlike)

	result, err := s.xiaohongshuService.LikeNotification(ctx, commentID, unlike)
	if err != nil {
		return &MCPToolResult{
			Content: []MCPContent{{Type: "text", Text: "点赞失败: " + err.Error()}},
			IsError: true,
		}
	}

	return marshalMCPResult(result, "点赞")
}
