package admin

import (
	"context"
	"errors"
	"time"

	"open_live_platform/internal/notifyemail"
)

type RoomModerationMail struct {
	ID          int64
	Lease       string
	DeliveryKey string
	Template    string
	Recipient   notifyemail.Address
	Fields      map[string]string
}

type RoomModerationMailStore interface {
	NextRoomModerationMail(context.Context) (RoomModerationMail, bool, error)
	CompleteRoomModerationMail(ctx context.Context, id int64, lease string, success bool) error
}

type RoomModerationMailSender interface {
	Send(context.Context, string, notifyemail.Address, map[string]string, string) error
}

func DeliverRoomModerationMail(ctx context.Context, store RoomModerationMailStore, sender RoomModerationMailSender) error {
	mail, found, err := store.NextRoomModerationMail(ctx)
	if err != nil || !found {
		return err
	}
	sendErr := sender.Send(ctx, mail.Template, mail.Recipient, mail.Fields, "room-moderation-"+mail.DeliveryKey)
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	storageErr := store.CompleteRoomModerationMail(finish, mail.ID, mail.Lease, sendErr == nil)
	return errors.Join(sendErr, storageErr)
}
