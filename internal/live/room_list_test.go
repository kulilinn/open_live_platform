package live

import (
	"math"
	"slices"
	"testing"
)

func TestRoomFilterWindow(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		page, size, total    int64
		offset, limit, pages int64
	}{
		{name: "empty", page: 1, size: 2},
		{name: "first page", page: 1, size: 2, total: 5, limit: 2, pages: 3},
		{name: "exact division", page: 2, size: 2, total: 4, offset: 2, limit: 2, pages: 2},
		{name: "partial last page", page: 3, size: 2, total: 5, offset: 4, limit: 1, pages: 3},
		{name: "out of range", page: 4, size: 2, total: 5, pages: 3},
		{name: "count only", page: math.MaxInt64, total: 5, pages: 5},
		{name: "maximum page size", page: 1, size: math.MaxInt64, total: 5, limit: 5, pages: 1},
		{name: "oversized offset", page: math.MaxInt64, size: math.MaxInt64, total: 5, pages: 1},
		{name: "maximum total", page: 2, size: math.MaxInt64 - 1, total: math.MaxInt64, offset: math.MaxInt64 - 1, limit: 1, pages: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := RoomFilter{Page: tc.page, PageSize: tc.size}
			offset, limit, pages := filter.Window(tc.total)
			if offset != tc.offset || limit != tc.limit || pages != tc.pages {
				t.Fatalf("Window(%d) = (%d, %d, %d), want (%d, %d, %d)", tc.total, offset, limit, pages, tc.offset, tc.limit, tc.pages)
			}
		})
	}
}

func TestListRooms(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	empty, pages, err := store.ListRooms(ctx, RoomFilter{Page: 1, PageSize: 2})
	if err != nil || empty == nil || len(empty) != 0 || pages != 0 {
		t.Fatalf("empty room list = %+v, %d, %v", empty, pages, err)
	}

	fixtures := make(map[int64]Room)
	for _, id := range []int64{3, 1, 5, 2, 4} {
		room, err := store.Start(ctx, id, "Live", "")
		requireError(t, err, nil)
		if id == 1 || id == 3 {
			requireError(t, store.Close(ctx, id, id), nil)
			room, err = store.Info(ctx, id)
			requireError(t, err, nil)
		}
		if id == 2 {
			requireError(t, store.Join(ctx, id, 6, ""), nil)
			room.MemberCount = 1
		}
		fixtures[id] = room
	}

	online, offline := true, false
	for _, tc := range []struct {
		name   string
		filter RoomFilter
		ids    []int64
		pages  int64
	}{
		{name: "first page", filter: RoomFilter{Page: 1, PageSize: 2}, ids: []int64{2, 4}, pages: 3},
		{name: "across online status", filter: RoomFilter{Page: 2, PageSize: 2}, ids: []int64{5, 1}, pages: 3},
		{name: "last page", filter: RoomFilter{Page: 3, PageSize: 2}, ids: []int64{3}, pages: 3},
		{name: "online", filter: RoomFilter{Online: &online, Page: 2, PageSize: 2}, ids: []int64{5}, pages: 2},
		{name: "offline", filter: RoomFilter{Online: &offline, Page: 1, PageSize: 2}, ids: []int64{1, 3}, pages: 1},
		{name: "out of range", filter: RoomFilter{Online: &offline, Page: math.MaxInt64, PageSize: 2}, pages: 1},
		{name: "count only", filter: RoomFilter{Online: &online, Page: 1}, pages: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := make([]Room, 0, len(tc.ids))
			for _, id := range tc.ids {
				want = append(want, fixtures[id])
			}
			rooms, pages, err := store.ListRooms(t.Context(), tc.filter)
			if err != nil || rooms == nil || !slices.Equal(rooms, want) || pages != tc.pages {
				t.Fatalf("ListRooms = %+v, %d, %v, want %+v, %d, nil", rooms, pages, err, want, tc.pages)
			}
		})
	}

	rooms, err := store.Rooms(ctx)
	want := []Room{fixtures[1], fixtures[2], fixtures[3], fixtures[4], fixtures[5]}
	if err != nil || !slices.Equal(rooms, want) {
		t.Fatalf("Rooms = %+v, %v, want %+v", rooms, err, want)
	}
}
