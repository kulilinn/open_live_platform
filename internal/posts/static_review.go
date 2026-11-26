package posts

import "context"

const (
	ReviewPlanName = iota + 1
	ReviewOfflineInfo
	ReviewWelcome
	ReviewRoomTitle
	ReviewRoomBanner
	ReviewWheelOption
	ReviewProfileBanner
	ReviewIntro
	ReviewAvatar
	ReviewDisplayName
	ReviewSearchName
)

type StaticReview struct {
	Kind               int
	CallbackObject     int64
	Slot               int
	Content, RequestID string
}

type staticReviewsContext struct{}

func WithStaticReviews(ctx context.Context, bindings []StaticReview) context.Context {
	if len(bindings) == 0 {
		return ctx
	}
	return context.WithValue(ctx, staticReviewsContext{}, bindings)
}

func StaticReviews(ctx context.Context) []StaticReview {
	reviews, _ := ctx.Value(staticReviewsContext{}).([]StaticReview)
	return reviews
}
