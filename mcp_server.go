// Modified from upstream: registers reliability-oriented MCP tools and schemas.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// Helper functions for annotation pointers
func boolPtr(b bool) *bool { return &b }

// MCP 工具参数结构体定义

// PublishContentArgs 发布内容的参数
type PublishContentArgs struct {
	Title          string   `json:"title" jsonschema:"内容标题（小红书限制：最多20个中文字或英文单词）"`
	Content        string   `json:"content" jsonschema:"正文内容，不包含以#开头的标签内容，所有话题标签都用tags参数来生成和提供即可"`
	Images         []string `json:"images" jsonschema:"图片路径列表（至少需要1张图片）。支持两种方式：1. HTTP/HTTPS图片链接（自动下载）；2. 本地图片绝对路径（推荐，如:/Users/user/image.jpg）"`
	Tags           []string `json:"tags,omitempty" jsonschema:"话题标签列表（可选参数），如 [美食, 旅行, 生活]"`
	ScheduleAt     string   `json:"schedule_at,omitempty" jsonschema:"定时发布时间（可选），ISO8601格式如 2024-01-20T10:30:00+08:00，支持1小时至14天内。不填则立即发布"`
	IsOriginal     bool     `json:"is_original,omitempty" jsonschema:"是否声明原创（可选），true为声明原创，false或不填则不声明"`
	Visibility     string   `json:"visibility,omitempty" jsonschema:"可见范围（可选），支持: 公开可见(默认)、仅自己可见、仅互关好友可见。不填则默认公开可见"`
	Products       []string `json:"products,omitempty" jsonschema:"商品关键词列表（可选），用于绑定带货商品。填写商品名称或商品ID，系统会自动搜索并选择第一个匹配结果。需账号已开通商品功能。示例: [面膜, 防晒霜SPF50]"`
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"调用方生成的唯一请求键（推荐 UUID）。相同键永远返回原任务，不会重复发布；不填时仅对完整发布参数哈希做10分钟去重"`
}

type GetPublishStatusArgs struct {
	TaskID string `json:"task_id" jsonschema:"publish_content 返回的任务 ID"`
}

// PublishVideoArgs 发布视频的参数（仅支持本地单个视频文件）
type PublishVideoArgs struct {
	Title      string   `json:"title" jsonschema:"内容标题（小红书限制：最多20个中文字或英文单词）"`
	Content    string   `json:"content" jsonschema:"正文内容，不包含以#开头的标签内容，所有话题标签都用tags参数来生成和提供即可"`
	Video      string   `json:"video" jsonschema:"本地视频绝对路径（仅支持单个视频文件，如:/Users/user/video.mp4）"`
	Tags       []string `json:"tags,omitempty" jsonschema:"话题标签列表（可选参数），如 [美食, 旅行, 生活]"`
	ScheduleAt string   `json:"schedule_at,omitempty" jsonschema:"定时发布时间（可选），ISO8601格式如 2024-01-20T10:30:00+08:00，支持1小时至14天内。不填则立即发布"`
	Visibility string   `json:"visibility,omitempty" jsonschema:"可见范围（可选），支持: 公开可见(默认)、仅自己可见、仅互关好友可见。不填则默认公开可见"`
	Products   []string `json:"products,omitempty" jsonschema:"商品关键词列表（可选），用于绑定带货商品。填写商品名称或商品ID，系统会自动搜索并选择第一个匹配结果。需账号已开通商品功能。示例: [面膜, 防晒霜SPF50]"`
}

// SearchFeedsArgs 搜索内容的参数
type SearchFeedsArgs struct {
	Keyword string       `json:"keyword" jsonschema:"搜索关键词"`
	Filters FilterOption `json:"filters,omitempty" jsonschema:"筛选选项"`
	Limit   int          `json:"limit,omitempty" jsonschema:"返回条数，默认20，最大30"`
}

// ListFeedsArgs 首页推荐列表参数。
type ListFeedsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"返回条数，默认20，最大30"`
}

// FilterOption 筛选选项结构体
type FilterOption struct {
	SortBy      string `json:"sort_by,omitempty" jsonschema:"排序依据: 综合|最新|最多点赞|最多评论|最多收藏,默认为'综合'"`
	NoteType    string `json:"note_type,omitempty" jsonschema:"笔记类型: 不限|视频|图文,默认为'不限'"`
	PublishTime string `json:"publish_time,omitempty" jsonschema:"发布时间: 不限|一天内|一周内|半年内,默认为'不限'"`
	SearchScope string `json:"search_scope,omitempty" jsonschema:"搜索范围: 不限|已看过|未看过|已关注,默认为'不限'"`
	Location    string `json:"location,omitempty" jsonschema:"位置距离: 不限|同城|附近,默认为'不限'"`
}

// FeedDetailArgs 获取Feed详情的参数
type FeedDetailArgs struct {
	FeedID           string `json:"feed_id" jsonschema:"小红书笔记ID，从Feed列表获取"`
	XsecToken        string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	LoadAllComments  bool   `json:"load_all_comments,omitempty" jsonschema:"是否加载全部评论。false仅返回前10条一级评论（默认），true滚动加载更多评论"`
	Limit            int    `json:"limit,omitempty" jsonschema:"【仅当load_all_comments为true时生效】限制加载的一级评论数量。例如20表示最多加载20条，默认20"`
	ClickMoreReplies bool   `json:"click_more_replies,omitempty" jsonschema:"【仅当load_all_comments为true时生效】是否展开二级回复。true展开子评论，false不展开（默认）"`
	ReplyLimit       int    `json:"reply_limit,omitempty" jsonschema:"【仅当click_more_replies为true时生效】跳过回复数过多的评论。例如10表示跳过超过10条回复的，默认10"`
	ScrollSpeed      string `json:"scroll_speed,omitempty" jsonschema:"【仅当load_all_comments为true时生效】滚动速度slow慢速、normal正常、fast快速"`
}

// UserProfileArgs 获取用户主页的参数
type UserProfileArgs struct {
	UserID    string `json:"user_id" jsonschema:"小红书用户ID，从Feed列表获取"`
	XsecToken string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	Tab       string `json:"tab,omitempty" jsonschema:"主页 tab: note(笔记,默认)|fav(收藏)|liked(点赞)。收藏和点赞可能被对方设为不公开"`
	Limit     int    `json:"limit,omitempty" jsonschema:"返回帖子条数，默认20，最大30"`
}

// MyProfileArgs 我的主页参数
type MyProfileArgs struct {
	Tab   string `json:"tab,omitempty" jsonschema:"主页 tab: note(笔记,默认)|fav(收藏)|liked(点赞)"`
	Limit int    `json:"limit,omitempty" jsonschema:"返回帖子条数，默认20，最大30"`
}

// PostCommentArgs 发表评论的参数
type PostCommentArgs struct {
	FeedID         string `json:"feed_id" jsonschema:"小红书笔记ID，从Feed列表获取"`
	XsecToken      string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	Content        string `json:"content" jsonschema:"评论内容"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"调用方生成的唯一请求键（推荐 UUID）。相同键永远返回原任务；不填时对完整参数做10分钟去重"`
}

type GetCommentStatusArgs struct {
	TaskID string `json:"task_id,omitempty" jsonschema:"可选；post_comment_to_feed 返回的任务 ID"`
	FeedID string `json:"feed_id,omitempty" jsonschema:"可选；task_id 丢失时按笔记 ID 反查历史顶层评论任务"`
}

// ReplyCommentArgs 回复评论的参数
type ReplyCommentArgs struct {
	FeedID         string `json:"feed_id" jsonschema:"小红书笔记ID，从Feed列表获取"`
	XsecToken      string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	CommentID      string `json:"comment_id" jsonschema:"必填；要回复的精确评论ID，可为主楼或楼中楼，从评论列表或list_notifications获取"`
	UserID         string `json:"user_id,omitempty" jsonschema:"可选交叉校验；绝不用于comment_id找不到时的兜底定位"`
	Content        string `json:"content" jsonschema:"回复内容"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"调用方生成的唯一请求键（推荐 UUID）。相同键永远返回原任务，不会重复回复；不填时对完整参数做10分钟去重"`
}

type GetReplyStatusArgs struct {
	TaskID    string `json:"task_id,omitempty" jsonschema:"可选；reply_comment_in_feed 返回的任务 ID"`
	FeedID    string `json:"feed_id,omitempty" jsonschema:"反查时的笔记 ID，必须与 comment_id 一起提供"`
	CommentID string `json:"comment_id,omitempty" jsonschema:"反查时的目标评论 ID，必须与 feed_id 一起提供"`
}

// LikeFeedArgs 点赞参数
type LikeFeedArgs struct {
	FeedID    string `json:"feed_id" jsonschema:"小红书笔记ID，从Feed列表获取"`
	XsecToken string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	Unlike    bool   `json:"unlike,omitempty" jsonschema:"是否取消点赞，true为取消点赞，false或未设置则为点赞"`
}

// FavoriteFeedArgs 收藏参数
type FavoriteFeedArgs struct {
	FeedID     string `json:"feed_id" jsonschema:"小红书笔记ID，从Feed列表获取"`
	XsecToken  string `json:"xsec_token" jsonschema:"访问令牌，从Feed列表的xsecToken字段获取"`
	Unfavorite bool   `json:"unfavorite,omitempty" jsonschema:"是否取消收藏，true为取消收藏，false或未设置则为收藏"`
}

// ListNotificationsArgs 通知列表参数
type ListNotificationsArgs struct {
	Tab   string `json:"tab,omitempty" jsonschema:"通知分区: mentions(评论和@,默认)|likes(赞和收藏)|connections(新增关注)"`
	Limit int    `json:"limit,omitempty" jsonschema:"返回条数上限，默认20"`
}

// LikeNotificationArgs 通知点赞参数
type LikeNotificationArgs struct {
	CommentID string `json:"comment_id" jsonschema:"目标评论ID，从 list_notifications 的 comment_id 字段获取"`
	Unlike    bool   `json:"unlike,omitempty" jsonschema:"是否取消点赞，true为取消点赞，false或未设置则为点赞"`
}

// ReplyNotificationArgs 通知回复参数
type ReplyNotificationArgs struct {
	CommentID string `json:"comment_id" jsonschema:"目标评论ID，从 list_notifications 的 comment_id 字段获取"`
	Content   string `json:"content" jsonschema:"回复内容"`
}

// InitMCPServer 初始化 MCP Server
func InitMCPServer(appServer *AppServer) *mcp.Server {
	// 创建 MCP Server
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "xiaohongshu-mcp",
			Version: "2.0.0",
		},
		nil,
	)

	// 注册所有工具
	registerTools(server, appServer)

	logrus.Info("MCP Server initialized with official SDK")

	return server
}

func withPanicRecovery[T any](
	toolName string,
	handler func(context.Context, *mcp.CallToolRequest, T) (*mcp.CallToolResult, any, error),
) func(context.Context, *mcp.CallToolRequest, T) (*mcp.CallToolResult, any, error) {

	return func(ctx context.Context, req *mcp.CallToolRequest, args T) (result *mcp.CallToolResult, resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logrus.WithFields(logrus.Fields{
					"tool":  toolName,
					"panic": r,
				}).Error("Tool handler panicked")

				logrus.Errorf("Stack trace:\n%s", debug.Stack())

				result = &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{
							Text: fmt.Sprintf("工具 %s 执行时发生内部错误: %v\n\n请查看服务端日志获取详细信息。", toolName, r),
						},
					},
					IsError: true,
				}
				resp = nil
				err = nil
			}
		}()

		return handler(ctx, req, args)
	}
}

// registerTools 注册所有 MCP 工具
func registerTools(server *mcp.Server, appServer *AppServer) {
	// 工具 1: 检查登录状态
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "check_login_status",
			Description: "检查小红书登录状态",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Check Login Status",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("check_login_status", func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			result := appServer.handleCheckLoginStatus(ctx)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 2: 获取登录二维码
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_login_qrcode",
			Description: "获取登录二维码（返回 Base64 图片和超时时间）",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Get Login QR Code",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("get_login_qrcode", func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetLoginQrcode(ctx)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 4: 发布内容
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "publish_content",
			Description: "异步提交小红书图文发布，立即返回任务 ID。必须用 get_publish_status 查询最终结果；客户端超时不能当作发布失败，也不要盲目重试",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Publish Content",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("publish_content", func(ctx context.Context, req *mcp.CallToolRequest, args PublishContentArgs) (*mcp.CallToolResult, any, error) {
			// 转换参数格式到现有的 handler
			argsMap := map[string]interface{}{
				"title":           args.Title,
				"content":         args.Content,
				"images":          convertStringsToInterfaces(args.Images),
				"tags":            convertStringsToInterfaces(args.Tags),
				"schedule_at":     args.ScheduleAt,
				"is_original":     args.IsOriginal,
				"visibility":      args.Visibility,
				"products":        convertStringsToInterfaces(args.Products),
				"idempotency_key": args.IdempotencyKey,
			}
			result := appServer.handlePublishContent(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 4b: 查询异步发布任务状态
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_publish_status",
			Description: "查询 publish_content 异步任务状态；为兼容尚未重新加载工具表的旧窗口，也可用 xhs_reply_ 或 xhs_comment_ 开头的任务 ID 查询回复或顶层评论任务。回复结果会返回phase、phase_updated_at和server_status；allowed_to_target只证明正确目标POST已放行，server_status才表示服务端是否明确接受或拒绝。阶段记录上线前的历史回复任务若缺少证据，非queued任务的phase=unknown；这不代表任务异常，应以status、request_status、server_status和result判断终态。unknown或uncertain表示最终结果无法确认，不得直接重发。",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Get Publish Status",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("get_publish_status", func(ctx context.Context, req *mcp.CallToolRequest, args GetPublishStatusArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetPublishStatus(ctx, args.TaskID)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 5: 获取Feed列表
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "list_feeds",
			Description: "获取精简的首页推荐列表，只返回作者、标题、转发/点赞/评论数及读取详情所需字段",
			Annotations: &mcp.ToolAnnotations{
				Title:        "List Feeds",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("list_feeds", func(ctx context.Context, req *mcp.CallToolRequest, args ListFeedsArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleListFeeds(ctx, args)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 6: 搜索内容
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "search_feeds",
			Description: "搜索小红书内容，返回精简的作者、标题、转发/点赞/评论数及详情和主页调用所需字段",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Search Feeds",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("search_feeds", func(ctx context.Context, req *mcp.CallToolRequest, args SearchFeedsArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleSearchFeeds(ctx, args)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 7: 获取Feed详情
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_feed_detail",
			Description: "获取小红书笔记详情，返回笔记内容、图片、作者信息、互动数据（点赞/收藏/分享数）及评论列表。视频笔记额外返回 video 字段，含各编码档位的视频直链与字幕地址（均带签名、有时效）。默认返回前10条一级评论，如需更多评论请设置load_all_comments=true",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Get Feed Detail",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("get_feed_detail", func(ctx context.Context, req *mcp.CallToolRequest, args FeedDetailArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"feed_id":           args.FeedID,
				"xsec_token":        args.XsecToken,
				"load_all_comments": args.LoadAllComments,
			}

			// 只有当 load_all_comments=true 时，才处理其他参数
			if args.LoadAllComments {
				argsMap["click_more_replies"] = args.ClickMoreReplies

				// 设置评论数量限制，默认20
				limit := args.Limit
				if limit <= 0 {
					limit = 20
				}
				argsMap["max_comment_items"] = limit

				// 设置回复数量阈值，默认10
				replyLimit := args.ReplyLimit
				if replyLimit <= 0 {
					replyLimit = 10
				}
				argsMap["max_replies_threshold"] = replyLimit

				if args.ScrollSpeed != "" {
					argsMap["scroll_speed"] = args.ScrollSpeed
				}
			}

			result := appServer.handleGetFeedDetail(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 8: 获取用户主页
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "user_profile",
			Description: "获取指定用户的精简主页资料、互动统计和精简帖子列表。tab 可选 note(笔记,默认)、fav(收藏)、liked(点赞)，后两者可能不公开",
			Annotations: &mcp.ToolAnnotations{
				Title:        "User Profile",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("user_profile", func(ctx context.Context, req *mcp.CallToolRequest, args UserProfileArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"user_id":    args.UserID,
				"xsec_token": args.XsecToken,
				"tab":        args.Tab,
				"limit":      args.Limit,
			}
			result := appServer.handleUserProfile(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 9: 发表评论
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "post_comment_to_feed",
			Description: "异步发表顶层评论，立即返回 xhs_comment_ 任务 ID；必须用 get_comment_status 查询终态。queued/running 尚未结束；confirmed 表示页面已确认评论出现；unknown 表示可能已发送，必须先核实且绝对不要盲目重发。支持显式幂等键，不填时对完整请求做10分钟去重。",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Post Comment",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("post_comment_to_feed", func(ctx context.Context, req *mcp.CallToolRequest, args PostCommentArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"feed_id":         args.FeedID,
				"xsec_token":      args.XsecToken,
				"content":         args.Content,
				"idempotency_key": args.IdempotencyKey,
			}
			result := appServer.handlePostComment(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_comment_status",
			Description: "查询异步顶层评论任务。可传 task_id 精确查询，或在 ID 丢失时传 feed_id 反查该笔记的全部历史任务（新者在前）。queued/running 必须继续查询；unknown 表示可能已发送，先核实评论区，绝对不要盲目重发。",
			Annotations: &mcp.ToolAnnotations{Title: "Get Comment Status", ReadOnlyHint: true},
		},
		withPanicRecovery("get_comment_status", func(ctx context.Context, req *mcp.CallToolRequest, args GetCommentStatusArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetCommentStatus(ctx, args)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 10: 回复评论
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "reply_comment_in_feed",
			Description: "异步按comment_id精确回复主楼或楼中楼，立即返回任务ID，必须用get_reply_status查询终态。同一feed_id+comment_id的queued/running始终阻止新建，10分钟内的uncertain/confirmed也会触发目标冷却，换正文或幂等键不能绕过。phase与phase_updated_at以单次事件显示locating_comment、target_located、reply_editor_open、reply_content_entered、submitted_pending_verification；reply_content_entered表示正文输入已完成、准备提交，最后一项表示网络闸门已向正确目标放行POST。提交按钮有独立短时限，不会耗尽整条任务时限。allowed_to_target只证明请求放行，server_status才表示服务端accepted/rejected/unknown/not_observed；status=rejected表示服务端明确拒绝且未发布成功。blocked/blocked_before_network表示提交前已阻断；uncertain表示可能已发送。页面可能嵌套新回复，也可能平铺到所属主楼树；人工核实必须同时检查目标楼层与整棵主楼树。reply_button_attempts显示按钮尝试次数。uncertain和通用超时绝对不要直接重发。user_id仅用于交叉校验。",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Reply Comment",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("reply_comment_in_feed", func(ctx context.Context, req *mcp.CallToolRequest, args ReplyCommentArgs) (*mcp.CallToolResult, any, error) {
			if args.CommentID == "" {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "缺少 comment_id；为避免错楼，user_id 不能用于兜底定位"}},
				}, nil, nil
			}

			argsMap := map[string]interface{}{
				"feed_id":         args.FeedID,
				"xsec_token":      args.XsecToken,
				"comment_id":      args.CommentID,
				"user_id":         args.UserID,
				"content":         args.Content,
				"idempotency_key": args.IdempotencyKey,
			}
			result := appServer.handleReplyComment(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 11: 查询回复任务。可用任务 ID，也可用 feed_id + comment_id 反查。
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_reply_status",
			Description: "查询异步回复状态，可传task_id，或用feed_id+comment_id反查该目标的全部操作（新者在前）。phase与phase_updated_at不是心跳；reply_content_entered表示正文输入已完成、准备提交，submitted_pending_verification表示正确目标POST已放行。提交按钮有独立短时限，不会耗尽整条任务时限。allowed_to_target只证明请求放行，server_status才表示服务端accepted/rejected/unknown/not_observed；status=rejected表示服务端明确拒绝且未发布成功。历史任务缺阶段证据时phase=unknown，这不代表任务异常，应以status、request_status、server_status和result判断。queued/running尚未结束；reply_button_attempts为2表示首次失败后已重新定位。uncertain表示可能已发送；页面可能嵌套也可能平铺，必须同时核实目标楼层和整棵所属主楼树，绝对不要重发。",
			Annotations: &mcp.ToolAnnotations{Title: "Get Reply Status", ReadOnlyHint: true},
		},
		withPanicRecovery("get_reply_status", func(ctx context.Context, req *mcp.CallToolRequest, args GetReplyStatusArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetReplyStatus(ctx, args)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 12: 发布视频（仅本地文件）
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "publish_with_video",
			Description: "发布小红书视频内容（仅支持本地单个视频文件）",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Publish Video",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("publish_with_video", func(ctx context.Context, req *mcp.CallToolRequest, args PublishVideoArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"title":       args.Title,
				"content":     args.Content,
				"video":       args.Video,
				"tags":        convertStringsToInterfaces(args.Tags),
				"schedule_at": args.ScheduleAt,
				"visibility":  args.Visibility,
				"products":    convertStringsToInterfaces(args.Products),
			}
			result := appServer.handlePublishVideo(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 12: 点赞笔记
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "like_feed",
			Description: "为指定笔记点赞或取消点赞（如已点赞将跳过点赞，如未点赞将跳过取消点赞）",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Like Feed",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("like_feed", func(ctx context.Context, req *mcp.CallToolRequest, args LikeFeedArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"feed_id":    args.FeedID,
				"xsec_token": args.XsecToken,
				"unlike":     args.Unlike,
			}
			result := appServer.handleLikeFeed(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 13: 收藏笔记
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "favorite_feed",
			Description: "收藏指定笔记或取消收藏（如已收藏将跳过收藏，如未收藏将跳过取消收藏）",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Favorite Feed",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("favorite_feed", func(ctx context.Context, req *mcp.CallToolRequest, args FavoriteFeedArgs) (*mcp.CallToolResult, any, error) {
			argsMap := map[string]interface{}{
				"feed_id":    args.FeedID,
				"xsec_token": args.XsecToken,
				"unfavorite": args.Unfavorite,
			}
			result := appServer.handleFavoriteFeed(ctx, argsMap)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 14: 获取我的主页
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_my_profile",
			Description: "获取当前登录用户的精简主页资料、互动统计和精简帖子列表。tab 可选 note(自己发的笔记,默认)、fav(自己收藏的)、liked(自己点赞的)",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Get My Profile",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("get_my_profile", func(ctx context.Context, req *mcp.CallToolRequest, args MyProfileArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetMyProfile(ctx, args.Tab, args.Limit)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 15: 通知未读数
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_unread_count",
			Description: "获取通知未读数，返回「评论和@」「赞和收藏」「新增关注」三个分区各自的未读条数。不会清除未读标记。查看具体内容用 list_notifications。",
			Annotations: &mcp.ToolAnnotations{
				Title:        "Get Unread Count",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("get_unread_count", func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			result := appServer.handleGetUnreadCount(ctx)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 16: 通知列表
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "list_notifications",
			Description: "获取通知列表。评论通知返回comment_level：top_level、sub_comment或unknown；unknown表示无法可靠证明层级，应先用get_feed_detail核实，绝不猜测。另返回评论者及对应笔记的feed_id和xsec_token。已删除或不可见条目会被过滤。注意：会清除该分区未读标记。",
			Annotations: &mcp.ToolAnnotations{
				Title:        "List Notifications",
				ReadOnlyHint: true,
			},
		},
		withPanicRecovery("list_notifications", func(ctx context.Context, req *mcp.CallToolRequest, args ListNotificationsArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleListNotifications(ctx, args.Tab, args.Limit)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 17: 回复通知里的评论
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "reply_notification",
			Description: "回复「评论和@」里的一条评论。comment_id 从 list_notifications 获取。适合处理收到的评论，无需先定位笔记。",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Reply Notification",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("reply_notification", func(ctx context.Context, req *mcp.CallToolRequest, args ReplyNotificationArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleReplyNotification(ctx, args.CommentID, args.Content)
			return convertToMCPResult(result), nil, nil
		}),
	)

	// 工具 18: 给通知里的评论点赞
	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "like_notification",
			Description: "给「评论和@」里的一条评论点赞或取消点赞。comment_id 从 list_notifications 获取（其 liked 字段是当前状态）。已是目标状态时跳过。",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Like Notification",
				DestructiveHint: boolPtr(true),
			},
		},
		withPanicRecovery("like_notification", func(ctx context.Context, req *mcp.CallToolRequest, args LikeNotificationArgs) (*mcp.CallToolResult, any, error) {
			result := appServer.handleLikeNotification(ctx, args.CommentID, args.Unlike)
			return convertToMCPResult(result), nil, nil
		}),
	)

	logrus.Infof("Registered %d MCP tools", 20)
}

// convertToMCPResult 将自定义的 MCPToolResult 转换为官方 SDK 的格式
func convertToMCPResult(result *MCPToolResult) *mcp.CallToolResult {
	var contents []mcp.Content
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			contents = append(contents, &mcp.TextContent{Text: c.Text})
		case "image":
			// 解码 base64 字符串为 []byte
			imageData, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				logrus.WithError(err).Error("Failed to decode base64 image data")
				// 如果解码失败，添加错误文本
				contents = append(contents, &mcp.TextContent{
					Text: "图片数据解码失败: " + err.Error(),
				})
			} else {
				contents = append(contents, &mcp.ImageContent{
					Data:     imageData,
					MIMEType: c.MimeType,
				})
			}
		}
	}

	return &mcp.CallToolResult{
		Content: contents,
		IsError: result.IsError,
	}
}

// convertStringsToInterfaces 辅助函数：将 []string 转换为 []interface{}
func convertStringsToInterfaces(strs []string) []interface{} {
	result := make([]interface{}, len(strs))
	for i, s := range strs {
		result[i] = s
	}
	return result
}
