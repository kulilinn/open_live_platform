package platformgrpc

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	platformv1 "open_live_platform/api/platform/v1"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

type userReader interface {
	Users(context.Context) ([]auth.User, error)
}

type roomReader interface {
	Rooms(context.Context) ([]live.Room, error)
}

type service struct {
	platformv1.UnimplementedPlatformServiceServer
	accounts userReader
	rooms    roomReader
}

// Register registers the platform statistics service on the internal gRPC server.
func Register(server grpc.ServiceRegistrar, accounts userReader, rooms roomReader) {
	platformv1.RegisterPlatformServiceServer(server, &service{accounts: accounts, rooms: rooms})
}

func (s *service) GetStats(ctx context.Context, _ *platformv1.GetStatsRequest) (*platformv1.GetStatsResponse, error) {
	users, err := s.accounts.Users(ctx)
	if err != nil {
		return nil, storeStatus(err, "could not load users")
	}

	rooms, err := s.rooms.Rooms(ctx)
	if err != nil {
		return nil, storeStatus(err, "could not load rooms")
	}

	result := make([]*platformv1.Room, 0, len(rooms))
	for _, room := range rooms {
		result = append(result, &platformv1.Room{
			Id:          room.ID,
			OwnerId:     room.OwnerID,
			Title:       room.Title,
			Online:      room.Online,
			MemberCount: int64(room.MemberCount),
			IsLock:      room.IsLock,
		})
	}
	return &platformv1.GetStatsResponse{Users: int64(len(users)), Rooms: result}, nil
}

func storeStatus(err error, message string) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.FromContextError(context.Canceled).Err()
	case errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(context.DeadlineExceeded).Err()
	default:
		return status.Error(codes.Internal, message)
	}
}
