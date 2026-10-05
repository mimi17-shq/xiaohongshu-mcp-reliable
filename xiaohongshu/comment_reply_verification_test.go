package xiaohongshu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingProtoClient struct {
	methods []string
	errors  map[string]error
	results map[string][]byte
}

func (c *recordingProtoClient) Call(_ context.Context, _, methodName string, _ interface{}) ([]byte, error) {
	c.methods = append(c.methods, methodName)
	return c.results[methodName], c.errors[methodName]
}

func pausedReplyRequest(target string) *proto.FetchRequestPaused {
	return &proto.FetchRequestPaused{
		RequestID: "request-1",
		Request: &proto.NetworkRequest{
			Method:   "POST",
			URL:      "https://edith.xiaohongshu.com/api/sns/web/v1/comment/post",
			PostData: `{"target_comment_id":"` + target + `"}`,
		},
	}
}

func replyTreeComment(id, userID, nickname string, children ...Comment) Comment {
	return Comment{
		ID: id,
		UserInfo: User{
			UserID:   userID,
			Nickname: nickname,
		},
		SubComments: children,
	}
}

func TestResolveReplyTargetTopLevelWithoutSubComments(t *testing.T) {
	target, err := resolveReplyTargetFromCommentTree([]Comment{
		replyTreeComment("top-empty", "user-1", "Top Empty"),
	}, "top-empty")

	require.NoError(t, err)
	assert.Equal(t, "top-empty", target.CommentID)
	assert.Equal(t, "top-empty", target.ThreadRoot)
	assert.Equal(t, "user-1", target.UserID)
	assert.Equal(t, "Top Empty", target.Nickname)
}

func TestResolveReplyTargetTopLevelWithSubComments(t *testing.T) {
	target, err := resolveReplyTargetFromCommentTree([]Comment{
		replyTreeComment("top-with-replies", "user-1", "Top", replyTreeComment("sub-1", "user-2", "Sub")),
	}, "top-with-replies")

	require.NoError(t, err)
	assert.Equal(t, "top-with-replies", target.CommentID)
	assert.Equal(t, "top-with-replies", target.ThreadRoot)
}

func TestResolveReplyTargetSubCommentUsesTopLevelRoot(t *testing.T) {
	target, err := resolveReplyTargetFromCommentTree([]Comment{
		replyTreeComment("top", "user-1", "Top",
			replyTreeComment("sub", "user-2", "Sub",
				replyTreeComment("nested-sub", "user-3", "Nested"))),
	}, "nested-sub")

	require.NoError(t, err)
	assert.Equal(t, "nested-sub", target.CommentID)
	assert.Equal(t, "top", target.ThreadRoot)
	assert.Equal(t, "user-3", target.UserID)
}

func TestResolveReplyTargetMissingAborts(t *testing.T) {
	target, err := resolveReplyTargetFromCommentTree([]Comment{
		replyTreeComment("top", "user-1", "Top", replyTreeComment("sub", "user-2", "Sub")),
	}, "missing-comment")

	assert.Nil(t, target)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "未找到目标 comment_id"))
}

func TestParseCommentPostTarget(t *testing.T) {
	assert.Equal(t, "target-1", parseCommentPostTarget(`{"note_id":"n1","target_comment_id":"target-1","content":"x"}`))
	assert.Empty(t, parseCommentPostTarget(`{"note_id":"n1","content":"x"}`))
	assert.Empty(t, parseCommentPostTarget(`not-json`))
}

func TestValidateReplyPostRequestAllowsExactTarget(t *testing.T) {
	target, allowed, reason := validateReplyPostRequest(
		"target-1",
		"POST",
		"https://edith.xiaohongshu.com/api/sns/web/v1/comment/post",
		`{"note_id":"n1","target_comment_id":"target-1","content":"x"}`,
	)

	assert.True(t, allowed)
	assert.Equal(t, "target-1", target)
	assert.Empty(t, reason)
}

func TestValidateReplyPostRequestBlocksWrongTarget(t *testing.T) {
	target, allowed, reason := validateReplyPostRequest(
		"expected",
		"POST",
		"https://edith.xiaohongshu.com/api/sns/web/v1/comment/post",
		`{"target_comment_id":"wrong"}`,
	)

	assert.False(t, allowed)
	assert.Equal(t, "wrong", target)
	assert.Contains(t, reason, "不一致")
}

func TestValidateReplyPostRequestBlocksMissingOrBrokenTarget(t *testing.T) {
	tests := []struct {
		name     string
		postData string
	}{
		{name: "missing", postData: `{"content":"x"}`},
		{name: "broken json", postData: `not-json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, allowed, reason := validateReplyPostRequest(
				"expected",
				"POST",
				"https://edith.xiaohongshu.com/api/sns/web/v1/comment/post",
				tt.postData,
			)
			assert.False(t, allowed)
			assert.Empty(t, target)
			assert.Contains(t, reason, "缺少可解析")
		})
	}
}

func TestValidateReplyPostRequestBlocksUnexpectedShape(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		requestURL string
	}{
		{name: "wrong method", method: "GET", requestURL: "https://edith.xiaohongshu.com/api/sns/web/v1/comment/post"},
		{name: "wrong endpoint", method: "POST", requestURL: "https://edith.xiaohongshu.com/api/sns/web/v1/other/post"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, allowed, reason := validateReplyPostRequest("expected", tt.method, tt.requestURL, `{"target_comment_id":"expected"}`)
			assert.False(t, allowed)
			assert.NotEmpty(t, reason)
		})
	}
}

func TestWaitReplyRequestGateTimesOutFailClosed(t *testing.T) {
	results := make(chan replyRequestGateResult)
	result, observed := waitReplyRequestGate(results, time.Millisecond)

	assert.False(t, observed)
	assert.Equal(t, replyRequestGateResult{}, result)
}

func TestReplyRequestStatusesDistinguishBlockedFromAllowedUnconfirmed(t *testing.T) {
	assert.Equal(t, ReplyVerificationStatus("rejected"), ReplyRejected)
	assert.Equal(t, ReplyVerificationStatus("blocked"), ReplyBlocked)
	assert.Equal(t, ReplyRequestStatus("blocked_before_network"), ReplyRequestBlocked)
	assert.Equal(t, ReplyRequestStatus("allowed_to_target"), ReplyRequestAllowed)
	assert.Equal(t, ReplyRequestStatus("not_observed"), ReplyRequestNotObserved)
	assert.Equal(t, ReplyServerStatus("accepted"), ReplyServerAccepted)
	assert.Equal(t, ReplyServerStatus("rejected"), ReplyServerRejected)
}

func TestClassifyCommentPostResponseAccepted(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"success":true}`),
		[]byte(`{"code":0}`),
		[]byte(`{"code":"success"}`),
	} {
		result := classifyCommentPostResponse(200, body)
		assert.Equal(t, ReplyServerAccepted, result.Status, "body=%s", body)
	}
}

func TestClassifyCommentPostResponseRejected(t *testing.T) {
	tests := []struct {
		status int
		body   string
	}{
		{status: 413, body: `{}`},
		{status: 200, body: `{"success":false}`},
		{status: 200, body: `{"code":300012}`},
	}
	for _, tt := range tests {
		result := classifyCommentPostResponse(tt.status, []byte(tt.body))
		assert.Equal(t, ReplyServerRejected, result.Status, "status=%d body=%s", tt.status, tt.body)
	}
}

func TestClassifyCommentPostResponseUnknownIsConservative(t *testing.T) {
	for _, body := range [][]byte{[]byte(`not-json`), []byte(`{}`)} {
		result := classifyCommentPostResponse(200, body)
		assert.Equal(t, ReplyServerUnknown, result.Status, "body=%s", body)
	}
}

func TestReplyVerificationTimeoutScalesWithContentAndCaps(t *testing.T) {
	short := replyVerificationTimeout("短回复")
	long := replyVerificationTimeout(strings.Repeat("长", 400))
	capped := replyVerificationTimeout(strings.Repeat("长", 5000))
	assert.Greater(t, long, short)
	assert.Equal(t, 45*time.Second, capped)
}

func TestReplySubmitClickHasIndependentShortTimeout(t *testing.T) {
	assert.Equal(t, 5*time.Second, replySubmitClickTimeout)
	assert.Less(t, replySubmitClickTimeout, commentActionTimeout(strings.Repeat("长", 400)))
}

func TestCommentActionTimeoutAllowsLongHumanizedInput(t *testing.T) {
	short := commentActionTimeout("短评")
	long := commentActionTimeout(strings.Repeat("长", 400))
	capped := commentActionTimeout(strings.Repeat("长", 5000))
	assert.Greater(t, long, short)
	assert.Equal(t, 5*time.Minute, capped)
}

func TestStopReplyBeforeSubmitReportsDefiniteNoSend(t *testing.T) {
	verification := &ReplyVerification{Status: ReplyUncertain, RequestStatus: ReplyRequestNotObserved}
	result, err := stopReplyBeforeSubmit(verification, "定位失败", errors.New("missing"))

	require.Error(t, err)
	assert.Same(t, verification, result)
	assert.Equal(t, ReplyBlocked, result.Status)
	assert.Equal(t, ReplyRequestNotStarted, result.RequestStatus)
	assert.Contains(t, result.Message, "确定没有发送")
}

func TestReplyButtonClickRetriesExactlyOnceAfterFailure(t *testing.T) {
	firstCalls, retryCalls, pauses := 0, 0, 0
	attempts, err := runReplyButtonClickWithRetry(
		func() error {
			firstCalls++
			return errors.New("detached")
		},
		func() error {
			retryCalls++
			return nil
		},
		func() { pauses++ },
	)
	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 1, retryCalls)
	assert.Equal(t, 1, pauses)
}

func TestReplyButtonClickDoesNotRetryAfterSuccess(t *testing.T) {
	retryCalls, pauses := 0, 0
	attempts, err := runReplyButtonClickWithRetry(
		func() error { return nil },
		func() error {
			retryCalls++
			return nil
		},
		func() { pauses++ },
	)
	require.NoError(t, err)
	assert.Equal(t, 1, attempts)
	assert.Zero(t, retryCalls)
	assert.Zero(t, pauses)
}

func TestReplyButtonClickReturnsBothFailuresAfterOneRetry(t *testing.T) {
	attempts, err := runReplyButtonClickWithRetry(
		func() error { return errors.New("first") },
		func() error { return errors.New("second") },
		func() {},
	)
	require.Error(t, err)
	assert.Equal(t, 2, attempts)
	assert.Contains(t, err.Error(), "first")
	assert.Contains(t, err.Error(), "second")
}

func TestHandlePausedReplyRequestContinuesOnlyExactTarget(t *testing.T) {
	client := &recordingProtoClient{errors: map[string]error{}}
	result := handlePausedReplyRequest(client, pausedReplyRequest("expected"), "expected", time.Now().Add(time.Second))

	assert.Equal(t, ReplyRequestAllowed, result.RequestStatus)
	assert.Equal(t, "expected", result.TargetComment)
	assert.NoError(t, result.Err)
	assert.Equal(t, []string{"Fetch.continueRequest"}, client.methods)
}

func TestHandlePausedReplyRequestFailsWrongTargetBeforeNetwork(t *testing.T) {
	client := &recordingProtoClient{errors: map[string]error{}}
	result := handlePausedReplyRequest(client, pausedReplyRequest("wrong"), "expected", time.Now().Add(time.Second))

	assert.Equal(t, ReplyRequestBlocked, result.RequestStatus)
	assert.Equal(t, "wrong", result.TargetComment)
	assert.Contains(t, result.Reason, "不一致")
	assert.Equal(t, []string{"Fetch.failRequest"}, client.methods)
}

func TestHandlePausedReplyRequestFailsClosedAfterDeadline(t *testing.T) {
	client := &recordingProtoClient{errors: map[string]error{}}
	result := handlePausedReplyRequest(client, pausedReplyRequest("expected"), "expected", time.Now().Add(-time.Millisecond))

	assert.Equal(t, ReplyRequestBlocked, result.RequestStatus)
	assert.Contains(t, result.Reason, "超过")
	assert.Equal(t, []string{"Fetch.failRequest"}, client.methods)
}

func TestHandlePausedReplyRequestBlocksWhenContinueFails(t *testing.T) {
	client := &recordingProtoClient{errors: map[string]error{
		"Fetch.continueRequest": errors.New("continue failed"),
	}}
	result := handlePausedReplyRequest(client, pausedReplyRequest("expected"), "expected", time.Now().Add(time.Second))

	assert.Equal(t, ReplyRequestBlocked, result.RequestStatus)
	assert.Contains(t, result.Reason, "放行失败")
	assert.Equal(t, []string{"Fetch.continueRequest", "Fetch.failRequest"}, client.methods)
}

func TestHandlePausedReplyResponseRecordsServerAcceptance(t *testing.T) {
	status := 200
	client := &recordingProtoClient{
		errors:  map[string]error{},
		results: map[string][]byte{"Fetch.getResponseBody": []byte(`{"body":"{\"code\":0}","base64Encoded":false}`)},
	}
	event := pausedReplyRequest("expected")
	event.ResponseStatusCode = &status
	result := handlePausedReplyResponse(client, event)
	assert.Equal(t, ReplyServerAccepted, result.Status)
	assert.Equal(t, 200, result.HTTPStatus)
	assert.Equal(t, "0", result.Code)
	assert.Equal(t, []string{"Fetch.getResponseBody", "Fetch.continueResponse"}, client.methods)
}

func TestHandlePausedReplyResponseRecordsExplicitRejection(t *testing.T) {
	status := 200
	client := &recordingProtoClient{
		errors:  map[string]error{},
		results: map[string][]byte{"Fetch.getResponseBody": []byte(`{"body":"{\"success\":false}","base64Encoded":false}`)},
	}
	event := pausedReplyRequest("expected")
	event.ResponseStatusCode = &status
	result := handlePausedReplyResponse(client, event)
	assert.Equal(t, ReplyServerRejected, result.Status)
	assert.Equal(t, []string{"Fetch.getResponseBody", "Fetch.continueResponse"}, client.methods)
}

func TestPotentialReplyPostCatchesEndpointOrPayloadChanges(t *testing.T) {
	assert.True(t, isPotentialReplyPost(pausedReplyRequest("expected")))
	assert.True(t, isPotentialReplyPost(&proto.FetchRequestPaused{
		RequestID: "request-2",
		Request: &proto.NetworkRequest{
			Method:   "POST",
			URL:      "https://edith.xiaohongshu.com/api/sns/web/v2/replies/create",
			PostData: `{"target_comment_id":"expected"}`,
		},
	}))
	assert.True(t, isPotentialReplyPost(&proto.FetchRequestPaused{
		RequestID: "request-3",
		Request: &proto.NetworkRequest{
			Method: "POST",
			URL:    "https://edith.xiaohongshu.com/api/sns/web/v2/comment/post",
		},
	}))
}

func TestPotentialReplyPostIgnoresUnrelatedTraffic(t *testing.T) {
	assert.False(t, isPotentialReplyPost(&proto.FetchRequestPaused{
		RequestID: "request-4",
		Request: &proto.NetworkRequest{
			Method: "GET",
			URL:    "https://edith.xiaohongshu.com/api/sns/web/v2/comment/page",
		},
	}))
	assert.False(t, isPotentialReplyPost(&proto.FetchRequestPaused{
		RequestID: "request-5",
		Request: &proto.NetworkRequest{
			Method:   "POST",
			URL:      "https://edith.xiaohongshu.com/api/telemetry",
			PostData: `{"event":"click"}`,
		},
	}))
}

func TestContinueUnrelatedRequestUsesFetchContinue(t *testing.T) {
	client := &recordingProtoClient{errors: map[string]error{}}
	err := continueUnrelatedRequest(client, &proto.FetchRequestPaused{RequestID: "request-6"})

	require.NoError(t, err)
	assert.Equal(t, []string{"Fetch.continueRequest"}, client.methods)
}

func TestClassifyReplyObservationConfirmed(t *testing.T) {
	before := map[string]replyObservation{
		"old": {CommentID: "old", ThreadRoot: "root-1", UserID: "me"},
	}
	after := map[string]replyObservation{
		"old": {CommentID: "old", ThreadRoot: "root-1", UserID: "me"},
		"new": {CommentID: "new", ThreadRoot: "root-1", UserID: "me"},
	}

	item, status := classifyReplyObservation(before, after, "root-1", "me")
	require.NotNil(t, item)
	assert.Equal(t, "new", item.CommentID)
	assert.Equal(t, ReplyConfirmed, status)
}

func TestClassifyReplyObservationWrongTarget(t *testing.T) {
	item, status := classifyReplyObservation(nil, map[string]replyObservation{
		"new": {CommentID: "new", ThreadRoot: "other-root", UserID: "me"},
	}, "expected-root", "me")

	require.NotNil(t, item)
	assert.Equal(t, "other-root", item.ThreadRoot)
	assert.Equal(t, ReplyWrongTarget, status)
}

func TestClassifyReplyObservationUncertain(t *testing.T) {
	before := map[string]replyObservation{
		"old": {CommentID: "old", ThreadRoot: "root-1", UserID: "me"},
	}
	after := map[string]replyObservation{
		"old": {CommentID: "old", ThreadRoot: "root-1", UserID: "me"},
	}

	item, status := classifyReplyObservation(before, after, "root-1", "me")
	assert.Nil(t, item)
	assert.Equal(t, ReplyUncertain, status)
}

func TestClassifyReplyObservationUnknownRootIsUncertain(t *testing.T) {
	item, status := classifyReplyObservation(nil, map[string]replyObservation{
		"new": {CommentID: "new", UserID: "me"},
	}, "expected-root", "me")

	assert.Nil(t, item)
	assert.Equal(t, ReplyUncertain, status)
}

func TestClassifyReplyObservationPrefersExpectedRoot(t *testing.T) {
	after := map[string]replyObservation{
		"wrong": {CommentID: "wrong", ThreadRoot: "other-root", UserID: "me"},
		"right": {CommentID: "right", ThreadRoot: "expected-root", UserID: "me"},
	}

	item, status := classifyReplyObservation(nil, after, "expected-root", "me")
	require.NotNil(t, item)
	assert.Equal(t, "right", item.CommentID)
	assert.Equal(t, ReplyConfirmed, status)
}

func TestClassifyReplyObservationIgnoresOtherUsers(t *testing.T) {
	item, status := classifyReplyObservation(nil, map[string]replyObservation{
		"same-text": {CommentID: "same-text", ThreadRoot: "expected-root", UserID: "someone-else"},
	}, "expected-root", "me")

	assert.Nil(t, item)
	assert.Equal(t, ReplyUncertain, status)
}
