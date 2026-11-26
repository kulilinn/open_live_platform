package admin

import (
	"context"
	"errors"
	"time"
)

type MCNInviteMail struct {
	ID                   int64
	Email, Subject, Body string
	Lease                string
}

type MCNMailStore interface {
	NextMCNInviteMail(context.Context) (MCNInviteMail, bool, error)
	CompleteMCNInviteMail(ctx context.Context, id int64, lease string, success bool) error
}

type MCNMailSender interface {
	SendPlainText(ctx context.Context, address, subject, body string) error
}

func DeliverMCNInviteMail(ctx context.Context, store MCNMailStore, sender MCNMailSender) error {
	mail, found, err := store.NextMCNInviteMail(ctx)
	if err != nil || !found {
		return err
	}
	sendErr := sender.SendPlainText(ctx, mail.Email, mail.Subject, mail.Body)
	next, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	storageErr := store.CompleteMCNInviteMail(next, mail.ID, mail.Lease, sendErr == nil)
	if sendErr != nil {
		sendErr = ErrMCNInviteUnavailable
	}
	return errors.Join(sendErr, storageErr)
}
