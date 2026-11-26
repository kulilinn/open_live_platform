package httpapi

import (
	"context"
	"net/http"

	"open_live_platform/internal/posts"
	"open_live_platform/internal/transport/httpx"
)

type staticContent struct {
	typeID                       int
	object                       int64
	slot                         int
	kind, before, after, refusal string
	image                        bool
}

func submitStaticReviews(w http.ResponseWriter, ctx context.Context, reviewer posts.Reviewer, actor int64, values ...staticContent) (context.Context, bool) {
	if reviewer == nil {
		return ctx, true
	}
	var tracked []posts.StaticReview
	for _, value := range values {
		if value.after == "" || value.after == value.before {
			continue
		}
		input := posts.ReviewRequest{Kind: value.kind, UserID: actor, PostID: value.object}
		if value.image {
			input.Media = []posts.Media{{URL: value.after}}
		} else {
			input.Text = value.after
		}
		var evaluation posts.Evaluation
		var err error
		if source, ok := reviewer.(posts.TrackedReviewer); ok {
			evaluation, err = source.Evaluate(ctx, input)
		} else {
			evaluation.Status, err = reviewer.Review(ctx, input)
		}
		if err != nil || evaluation.Status == posts.Pending {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4069", "message": "content audit unavailable"})
			return ctx, false
		}
		if evaluation.Status != posts.Approved {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": value.refusal, "message": "content audit rejected"})
			return ctx, false
		}
		if evaluation.ManualRequestID != "" {
			tracked = append(tracked, posts.StaticReview{Kind: value.typeID, CallbackObject: value.object, Slot: value.slot,
				Content: value.after, RequestID: evaluation.ManualRequestID})
		}
	}
	return posts.WithStaticReviews(ctx, tracked), true
}

func optionalStaticValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
