package platformgrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	platformv1 "open_live_platform/api/platform/v1"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func TestGetStatsRoundTrip(t *testing.T) {
	const roomID, ownerID = 1<<54 + 27, 1<<53 + 39
	checkContext := func(ctx context.Context) {
		t.Helper()
		if values := metadata.ValueFromIncomingContext(ctx, "stats-test"); len(values) != 1 || values[0] != "request" {
			t.Errorf("incoming metadata = %q, want [request]", values)
		}
	}
	store := statsStore{
		users: func(ctx context.Context) ([]auth.User, error) {
			checkContext(ctx)
			return []auth.User{{ID: 17}, {ID: ownerID}}, nil
		},
		rooms: func(ctx context.Context) ([]live.Room, error) {
			checkContext(ctx)
			return []live.Room{
				{ID: roomID, OwnerID: ownerID, Title: "夜の配信", Online: true, MemberCount: 3, IsLock: true},
				{ID: 17, OwnerID: 17, Title: "Offline"},
			}, nil
		},
	}
	server := grpc.NewServer()
	listener := bufconn.Listen(1 << 20)
	t.Cleanup(server.Stop)
	Register(server, store, store)
	go server.Serve(listener)

	conn, err := grpc.NewClient("passthrough:///platform", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "stats-test", "request")
	got, err := platformv1.NewPlatformServiceClient(conn).GetStats(ctx, &platformv1.GetStatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := &platformv1.GetStatsResponse{
		Users: 2,
		Rooms: []*platformv1.Room{
			{Id: roomID, OwnerId: ownerID, Title: "夜の配信", Online: true, MemberCount: 3, IsLock: true},
			{Id: 17, OwnerId: 17, Title: "Offline"},
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("GetStats = %v, want %v", got, want)
	}
}

func TestGetStatsStoreErrors(t *testing.T) {
	backendErr := errors.New("backend connection detail")
	for _, tc := range []struct {
		name     string
		usersErr error
		roomsErr error
		code     codes.Code
		message  string
	}{
		{"users", backendErr, nil, codes.Internal, "could not load users"},
		{"rooms", nil, backendErr, codes.Internal, "could not load rooms"},
		{"canceled", fmt.Errorf("users query: %w", context.Canceled), nil, codes.Canceled, "context canceled"},
		{"deadline", nil, fmt.Errorf("rooms query: %w", context.DeadlineExceeded), codes.DeadlineExceeded, "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := statsStore{
				users: func(context.Context) ([]auth.User, error) { return nil, tc.usersErr },
				rooms: func(context.Context) ([]live.Room, error) {
					if tc.usersErr != nil {
						t.Error("room store called after account store error")
					}
					return nil, tc.roomsErr
				},
			}
			s := service{accounts: store, rooms: store}
			got, err := s.GetStats(t.Context(), &platformv1.GetStatsRequest{})
			if got != nil || status.Code(err) != tc.code || status.Convert(err).Message() != tc.message {
				t.Errorf("GetStats = %v, %v, want nil, %v (%q)", got, err, tc.code, tc.message)
			}
		})
	}
}

type statsStore struct {
	users func(context.Context) ([]auth.User, error)
	rooms func(context.Context) ([]live.Room, error)
}

func (s statsStore) Users(ctx context.Context) ([]auth.User, error) { return s.users(ctx) }
func (s statsStore) Rooms(ctx context.Context) ([]live.Room, error) { return s.rooms(ctx) }
