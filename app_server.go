// Modified from upstream: adds durable asynchronous task managers.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// AppServer 应用服务器结构体，封装所有服务和处理器
type AppServer struct {
	xiaohongshuService *XiaohongshuService
	publishTasks       *PublishTaskManager
	replyTasks         *ReplyTaskManager
	commentTasks       *CommentTaskManager
	mcpServer          *mcp.Server
	router             *gin.Engine
	httpServer         *http.Server
	authToken          string
}

// NewAppServer 创建新的应用服务器实例
func NewAppServer(xiaohongshuService *XiaohongshuService, authToken string) *AppServer {
	publishTasks, err := NewPublishTaskManager(defaultPublishTaskPath(), xiaohongshuService.PublishContent)
	if err != nil {
		panic(fmt.Sprintf("初始化发布任务管理器失败: %v", err))
	}
	replyTasks, err := NewReplyTaskManager(defaultReplyTaskPath(), xiaohongshuService.ReplyCommentToFeedWithProgress)
	if err != nil {
		panic(fmt.Sprintf("初始化回复任务管理器失败: %v", err))
	}
	commentTasks, err := NewCommentTaskManager(defaultCommentTaskPath(), xiaohongshuService.PostCommentToFeed)
	if err != nil {
		panic(fmt.Sprintf("初始化顶层评论任务管理器失败: %v", err))
	}
	appServer := &AppServer{
		xiaohongshuService: xiaohongshuService,
		publishTasks:       publishTasks,
		replyTasks:         replyTasks,
		commentTasks:       commentTasks,
		authToken:          authToken,
	}

	// 初始化 MCP Server（需要在创建 appServer 之后，因为工具注册需要访问 appServer）
	appServer.mcpServer = InitMCPServer(appServer)

	return appServer
}

// Start 启动服务器
func (s *AppServer) Start(port string) error {
	if err := s.publishTasks.Start(); err != nil {
		return fmt.Errorf("启动发布任务管理器失败: %w", err)
	}
	if err := s.replyTasks.Start(); err != nil {
		return fmt.Errorf("启动回复任务管理器失败: %w", err)
	}
	if err := s.commentTasks.Start(); err != nil {
		return fmt.Errorf("启动顶层评论任务管理器失败: %w", err)
	}
	s.router = setupRoutes(s)

	s.httpServer = &http.Server{
		Addr:    port,
		Handler: s.router,
	}

	// 启动服务器的 goroutine
	go func() {
		logrus.Infof("启动 HTTP 服务器: %s", port)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Errorf("服务器启动失败: %v", err)
			os.Exit(1)
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logrus.Infof("正在关闭服务器...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		logrus.Warnf("等待连接关闭超时，强制退出: %v", err)
	} else {
		logrus.Infof("服务器已优雅关闭")
	}

	return nil
}
