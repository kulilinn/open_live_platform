package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type subscriptionMemberView struct {
	ID    int64  `json:"uid,string"`
	Photo string `json:"img"`
	State int    `json:"state"`
}

func (s subscriptionAPI) groups(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Kind int `json:"ty"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.Kind < subscription.FreeGroup || input.Kind > subscription.CustomGroup {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	ctx := r.Context()
	groups, err := s.store.SubscriptionGroups(ctx, actor, input.Kind)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	result := make(map[string][3]any, len(groups))
	for _, group := range groups {
		users, err := s.directoryMemberViews(ctx, group.Users[:min(len(group.Users), 3)])
		if err != nil {
			subscriptionError(w, err)
			return
		}
		result[strconv.FormatInt(group.ID, 10)] = [3]any{group.Name, users, 0}
	}
	subscriptionResult(w, map[string]any{"groups": result})
}

func (s subscriptionAPI) createGroup(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Name string `json:"group_name"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if !validGroupName(input.Name) {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	writeDirectoryChange(w, s.store.CreateSubscriptionGroup(r.Context(), actor, input.Name), true)
}

func validGroupName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= 128
}

func (s subscriptionAPI) changeGroup(operation string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var input struct {
			ID   subscriptionID `json:"group_id"`
			Name string         `json:"group_name"`
			List int            `json:"ty"`
		}
		if !decodeEmailRequest(w, r, &input) {
			return
		}
		ctx := r.Context()
		change := subscription.GroupChange{Clear: operation == "clear", Delete: operation == "delete"}
		if change.Delete && input.ID == 0 {
			if input.List == subscription.SubscriptionList {
				writeDirectoryChange(w, nil, true)
				return
			}
			if input.List == subscription.Friends || input.List == subscription.Blacklist {
				writeDirectoryChange(w, s.store.ClearSubscriptionList(ctx, actor, input.List), true)
				return
			}
		}
		if input.ID < 1 || (operation == "rename" && !validGroupName(input.Name)) {
			subscriptionError(w, subscription.ErrInvalidPlan)
			return
		}
		if operation == "rename" {
			change.Name = &input.Name
		}
		writeDirectoryChange(w, s.store.ChangeSubscriptionGroup(ctx, actor, int64(input.ID), change), true)
	}
}

func (s subscriptionAPI) changeGroupMember(add bool) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		var input struct {
			GroupID  subscriptionID `json:"group_id"`
			PersonID subscriptionID `json:"uid"`
			List     int            `json:"ty"`
		}
		if !decodeEmailRequest(w, r, &input) {
			return
		}
		person := int64(input.PersonID)
		if person < 1 || person == actor || (input.List != 0 && input.List != subscription.Friends && input.List != subscription.Blacklist) ||
			(input.GroupID == 0 && input.List == 0) || (input.GroupID != 0 && input.List == subscription.Blacklist) {
			subscriptionError(w, subscription.ErrInvalidPlan)
			return
		}
		ctx := r.Context()
		if add {
			_, found, err := s.accounts.Find(ctx, person)
			if err != nil || !found {
				if err == nil {
					err = subscription.ErrNotFound
				}
				subscriptionError(w, err)
				return
			}
		}
		change := subscription.MemberChange{GroupID: int64(input.GroupID), PersonID: person, List: input.List, Add: add}
		writeDirectoryChange(w, s.store.ChangeSubscriptionMember(ctx, actor, change), false)
	}
}

func (s subscriptionAPI) directoryMemberViews(ctx context.Context, ids []int64) ([]subscriptionMemberView, error) {
	result := make([]subscriptionMemberView, 0, len(ids))
	for _, id := range ids {
		user, found, err := s.accounts.Find(ctx, id)
		if err != nil {
			return nil, err
		}
		if found {
			result = append(result, subscriptionMemberView{ID: id, Photo: subscriptionDMUser(user, false).Photo})
		}
	}
	return result, nil
}

func (s subscriptionAPI) savedUsers(list int) userHandler {
	return func(w http.ResponseWriter, r *http.Request, actor int64) {
		ctx := r.Context()
		ids, err := s.store.SavedSubscriptionUsers(ctx, actor, list)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		users, err := s.directoryMemberViews(ctx, ids)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		data := any(users)
		if list == subscription.Blacklist {
			data = map[string]any{"list": users}
		}
		writeSubscriptionData(w, data)
	}
}

func (s subscriptionAPI) listAnchors(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		GroupID subscriptionID `json:"group_id"`
		List    int            `json:"ty"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	ctx := r.Context()
	rows, err := s.store.Relationships(ctx, actor, 0)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	var ids []int64
	switch {
	case input.GroupID > 0:
		var group subscription.Group
		group, err = s.store.SubscriptionGroup(ctx, actor, int64(input.GroupID))
		if errors.Is(err, subscription.ErrNotFound) {
			err = nil
		}
		ids = group.Users
	case input.List == subscription.SubscriptionList:
		ids, err = s.activeAnchors(ctx, actor, rows)
	case input.List == subscription.Friends || input.List == subscription.Blacklist:
		ids, err = s.store.SavedSubscriptionUsers(ctx, actor, input.List)
	case input.List != 0:
		err = subscription.ErrInvalidPlan
	}
	if err != nil {
		subscriptionError(w, err)
		return
	}
	byAnchor := map[int64][]subscription.Relationship{}
	for _, sub := range rows {
		byAnchor[sub.AnchorID] = append(byAnchor[sub.AnchorID], sub)
	}
	result := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		account, found, err := s.accounts.Find(ctx, id)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		if !found {
			continue
		}
		room, err := s.rooms.Info(ctx, id)
		if err != nil && !errors.Is(err, live.ErrRoomNotFound) {
			subscriptionError(w, err)
			return
		}
		other := subscriptionDMUser(account, false)
		name := ""
		if account.Profile != nil {
			name = account.Profile.Name
		}
		planRows := byAnchor[id]
		if planRows == nil {
			planRows = []subscription.Relationship{}
		}
		subList, err := json.Marshal(planRows)
		if err != nil {
			subscriptionError(w, err)
			return
		}
		result = append(result, map[string]any{"id": strconv.FormatInt(id, 10), "name": name,
			"search_name": other.SearchName, "display_name": other.DisplayName, "img": other.Photo,
			"banner": room.Banner, "info": room.Description, "state": 0, "price": 0, "favorite": 0,
			"is_expired": 0, "sub": string(subList)})
	}
	writeSubscriptionData(w, map[string]any{"anchors": result})
}

func (s subscriptionAPI) activeAnchors(ctx context.Context, actor int64, rows []subscription.Relationship) ([]int64, error) {
	blocked, err := s.store.SavedSubscriptionUsers(ctx, actor, subscription.Blacklist)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	result := []int64{}
	seen := make(map[int64]struct{})
	for _, sub := range rows {
		_, known := seen[sub.AnchorID]
		_, hidden := slices.BinarySearch(blocked, sub.AnchorID)
		if !known && !hidden && sub.Active(now) {
			seen[sub.AnchorID] = struct{}{}
			result = append(result, sub.AnchorID)
		}
	}
	return result, nil
}

func (s subscriptionAPI) subscriptionIndex(w http.ResponseWriter, r *http.Request, actor int64) {
	ctx := r.Context()
	subs, err := s.store.Relationships(ctx, actor, 0)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	ids, err := s.activeAnchors(ctx, actor, subs)
	if err != nil {
		subscriptionError(w, err)
		return
	}
	result := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		result = append(result, map[string]string{"anchor_id": strconv.FormatInt(id, 10)})
	}
	writeSubscriptionData(w, result)
}

func (s postAPI) reportSubscription(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Kind   int            `json:"ty"`
		URLs   []string       `json:"url"`
		Remark string         `json:"remark"`
		Person subscriptionID `json:"user_id"`
		Post   subscriptionID `json:"post_id"`
	}
	if !decodeEmailRequest(w, r, &input) {
		return
	}
	if input.Kind < 1 || input.Kind > 5 || len(input.URLs) > 20 || utf8.RuneCountInString(input.Remark) > 2000 {
		subscriptionError(w, subscription.ErrInvalidPlan)
		return
	}
	for _, url := range input.URLs {
		if len(url) > 2048 || !postAssetURL(url) {
			subscriptionError(w, subscription.ErrInvalidPlan)
			return
		}
	}
	if input.URLs == nil {
		input.URLs = []string{}
	}
	ctx := r.Context()
	if input.Person != 0 {
		if _, found, err := s.accounts.Find(ctx, int64(input.Person)); err != nil || !found {
			if err == nil {
				err = subscription.ErrNotFound
			}
			subscriptionError(w, err)
			return
		}
	}
	if input.Post != 0 {
		if _, err := s.entries.GetPost(ctx, actor, int64(input.Post)); err != nil {
			postError(w, err)
			return
		}
	}
	report := subscription.Report{Reporter: actor, PersonID: int64(input.Person), PostID: int64(input.Post),
		Kind: input.Kind, URLs: input.URLs, Remark: input.Remark}
	writeDirectoryChange(w, s.subscriptions.ReportSubscription(ctx, report), false)
}

func (s subscriptionAPI) legacyPlanChange(w http.ResponseWriter, _ *http.Request, _ int64) {
	subscriptionStatus(w, nil)
}

func writeDirectoryChange(w http.ResponseWriter, err error, result bool) {
	if err == nil && result {
		subscriptionResult(w, map[string]any{})
		return
	}
	subscriptionStatus(w, err)
}

func writeSubscriptionData(w http.ResponseWriter, data any) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}

func subscriptionResult(w http.ResponseWriter, result any) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": result})
}
