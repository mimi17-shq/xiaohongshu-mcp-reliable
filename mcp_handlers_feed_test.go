package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestNormalizeFeedListLimit(t *testing.T) {
	require.Equal(t, 20, normalizeFeedListLimit(0))
	require.Equal(t, 20, normalizeFeedListLimit(-1))
	require.Equal(t, 1, normalizeFeedListLimit(1))
	require.Equal(t, 30, normalizeFeedListLimit(30))
	require.Equal(t, 30, normalizeFeedListLimit(31))
}

func TestSlimFeeds(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{
			ID:        "feed-1",
			XsecToken: "token-1",
			ModelType: "note",
			Index:     7,
			NoteCard: xiaohongshu.NoteCard{
				DisplayTitle: "title-1",
				User: xiaohongshu.User{
					UserID:   "user-1",
					Nickname: "author-1",
					Avatar:   "must-not-leak",
				},
				InteractInfo: xiaohongshu.InteractInfo{
					SharedCount:  "3",
					LikedCount:   "4",
					CommentCount: "5",
				},
				Cover: xiaohongshu.Cover{URL: "must-not-leak"},
			},
		},
		{
			ID:        "feed-2",
			XsecToken: "token-2",
			NoteCard: xiaohongshu.NoteCard{
				DisplayTitle: "title-2",
				User:         xiaohongshu.User{NickName: "fallback-author"},
			},
		},
	}

	got := slimFeeds(feeds, 1)
	require.Equal(t, 2, got.TotalCount)
	require.Equal(t, 1, got.ReturnedCount)
	require.Equal(t, []FeedListItem{{
		FeedID:       "feed-1",
		XsecToken:    "token-1",
		UserID:       "user-1",
		Author:       "author-1",
		Title:        "title-1",
		ShareCount:   "3",
		LikeCount:    "4",
		CommentCount: "5",
	}}, got.Feeds)

	fallback := slimFeeds(feeds[1:], 20)
	require.Equal(t, "fallback-author", fallback.Feeds[0].Author)
}

func TestSlimUserProfile(t *testing.T) {
	result := &UserProfileResponse{
		UserBasicInfo: xiaohongshu.UserBasicInfo{
			Nickname:   "nickname",
			Desc:       "description",
			IpLocation: "location",
			RedId:      "red-id",
			Imageb:     "must-not-leak",
			Images:     "must-not-leak",
		},
		Interactions: []xiaohongshu.UserInteractions{{Type: "fans", Name: "粉丝", Count: "8"}},
		Feeds: []xiaohongshu.Feed{{ID: "feed", XsecToken: "token", NoteCard: xiaohongshu.NoteCard{
			DisplayTitle: "title",
			User:         xiaohongshu.User{UserID: "user", Nickname: "author"},
		}}},
	}

	got := slimUserProfile(result, 20)
	require.Equal(t, SlimUserBasicInfo{
		Nickname: "nickname", Description: "description", Location: "location", RedID: "red-id",
	}, got.Profile)
	require.Equal(t, 1, got.TotalCount)
	require.Equal(t, 1, got.ReturnedCount)
	require.Equal(t, "user", got.Feeds[0].UserID)
}
