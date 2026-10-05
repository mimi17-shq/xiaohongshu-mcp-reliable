package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplyCommentRequiresExactCommentID(t *testing.T) {
	router := setupRoutes(NewAppServer(NewXiaohongshuService(), ""))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)

	var response struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	var replyFound, statusFound, publishStatusFound bool
	for _, tool := range response.Result.Tools {
		switch tool.Name {
		case "get_publish_status":
			publishStatusFound = true
			require.Contains(t, tool.Description, "phase_updated_at")
			require.Contains(t, tool.Description, "xhs_reply_")
			require.Contains(t, tool.Description, "phase=unknown")
			require.Contains(t, tool.Description, "不代表任务异常")
			require.Contains(t, tool.Description, "status、request_status、server_status和result")
			require.Contains(t, tool.Description, "xhs_comment_")
			require.Contains(t, tool.Description, "server_status")
		case "reply_comment_in_feed":
			replyFound = true
			require.Contains(t, tool.InputSchema.Required, "comment_id")
			require.NotContains(t, tool.InputSchema.Required, "user_id")
			require.Contains(t, tool.InputSchema.Properties, "idempotency_key")
			require.Contains(t, tool.Description, "blocked_before_network")
			require.Contains(t, tool.Description, "uncertain")
			require.Contains(t, tool.Description, "平铺")
			require.Contains(t, tool.Description, "嵌套")
			require.Contains(t, tool.Description, "目标楼层")
			require.Contains(t, tool.Description, "主楼")
			require.Contains(t, tool.Description, "reply_button_attempts")
			require.Contains(t, tool.Description, "phase_updated_at")
			require.Contains(t, tool.Description, "locating_comment")
			require.Contains(t, tool.Description, "reply_content_entered")
			require.Contains(t, tool.Description, "submitted_pending_verification")
			require.Contains(t, tool.Description, "独立短时限")
			require.Contains(t, tool.Description, "目标冷却")
			require.Contains(t, tool.Description, "server_status")
			require.Contains(t, tool.Description, "rejected")
		case "get_reply_status":
			statusFound = true
			require.Contains(t, tool.InputSchema.Properties, "task_id")
			require.Contains(t, tool.InputSchema.Properties, "feed_id")
			require.Contains(t, tool.InputSchema.Properties, "comment_id")
			require.Contains(t, tool.Description, "uncertain")
			require.Contains(t, tool.Description, "嵌套")
			require.Contains(t, tool.Description, "目标楼层")
			require.Contains(t, tool.Description, "主楼")
			require.Contains(t, tool.Description, "reply_button_attempts")
			require.Contains(t, tool.Description, "phase_updated_at")
			require.Contains(t, tool.Description, "reply_content_entered")
			require.Contains(t, tool.Description, "submitted_pending_verification")
			require.Contains(t, tool.Description, "独立短时限")
			require.Contains(t, tool.Description, "phase=unknown")
			require.Contains(t, tool.Description, "不代表任务异常")
			require.Contains(t, tool.Description, "status、request_status、server_status和result")
			require.Contains(t, tool.Description, "server_status")
			require.Contains(t, tool.Description, "rejected")
		}
	}
	require.True(t, publishStatusFound, "get_publish_status not registered")
	require.True(t, replyFound, "reply_comment_in_feed not registered")
	require.True(t, statusFound, "get_reply_status not registered")
}
