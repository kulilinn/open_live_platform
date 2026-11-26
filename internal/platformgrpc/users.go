package platformgrpc

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	platformv1 "open_live_platform/api/platform/v1"
	"open_live_platform/internal/auth"
)

type directoryService struct {
	platformv1.UnimplementedUserDirectoryServiceServer
	directory auth.UserDirectory
}

func RegisterUserDirectory(server grpc.ServiceRegistrar, directory auth.UserDirectory) {
	platformv1.RegisterUserDirectoryServiceServer(server, &directoryService{directory: directory})
}

func (s *directoryService) GetUser(ctx context.Context, request *platformv1.GetUserRequest) (*platformv1.DirectoryUser, error) {
	service := metadata.ValueFromIncomingContext(ctx, "x-inner-service")
	authorization := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(service) != 1 || len(authorization) != 1 || !s.directory.Authorized(service[0], authorization[0]) {
		return nil, status.Error(codes.PermissionDenied, "untrusted user service")
	}
	if request.GetId() < 1 {
		return nil, status.Error(codes.InvalidArgument, "invalid user ID")
	}
	profile, err := s.directory.Get(ctx, request.GetId())
	if errors.Is(err, auth.ErrDirectoryUserNotFound) {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if err != nil {
		return nil, storeStatus(err, "could not read user")
	}
	return &platformv1.DirectoryUser{Id: profile.ID, Email: profile.Email, Name: profile.Name, DisplayName: profile.DisplayName,
		SearchName: profile.SearchName, Language: profile.Language, Photo: profile.Photo, Banner: profile.Banner,
		IsAnchor: profile.IsAnchor != 0, Status: int32(profile.Status)}, nil
}
