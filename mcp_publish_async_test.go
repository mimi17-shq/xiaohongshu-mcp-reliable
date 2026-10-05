package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAsyncPublishToolsAndIdempotencySchemaRegistered(t *testing.T) {
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
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Result.Tools, 20)

	var publishFound, statusFound, commentFound, commentStatusFound bool
	for _, tool := range response.Result.Tools {
		switch tool.Name {
		case "publish_content":
			publishFound = true
			_, hasKey := tool.InputSchema.Properties["idempotency_key"]
			require.True(t, hasKey, "publish_content must expose idempotency_key")
		case "get_publish_status":
			statusFound = true
			_, hasTaskID := tool.InputSchema.Properties["task_id"]
			require.True(t, hasTaskID, "get_publish_status must require a task_id argument")
		case "post_comment_to_feed":
			commentFound = true
			_, hasKey := tool.InputSchema.Properties["idempotency_key"]
			require.True(t, hasKey, "post_comment_to_feed must expose idempotency_key")
		case "get_comment_status":
			commentStatusFound = true
			require.Contains(t, tool.InputSchema.Properties, "task_id")
			require.Contains(t, tool.InputSchema.Properties, "feed_id")
		}
	}
	require.True(t, publishFound)
	require.True(t, statusFound)
	require.True(t, commentFound)
	require.True(t, commentStatusFound)
}
