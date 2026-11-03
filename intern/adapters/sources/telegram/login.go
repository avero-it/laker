package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// Asker reads what only the user knows: the login code Telegram sends and
// the two-step verification password.
type Asker interface {
	Code() (string, error)
	Password() (string, error)
}

type flowAuth struct {
	phone string
	ask   Asker
}

func (f flowAuth) Phone(context.Context) (string, error)    { return f.phone, nil }
func (f flowAuth) Password(context.Context) (string, error) { return f.ask.Password() }
func (f flowAuth) Code(context.Context, *tg.AuthSentCode) (string, error) {
	return f.ask.Code()
}
func (f flowAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error { return nil }
func (f flowAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("ERR254: this phone has no Telegram account")
}

// Login creates the session of the adapter: Telegram sends a code to the
// account, the user types it (and the password, with two-step verification).
// The session file (mode 0600) stays on this machine.
func (s *Source) Login(ctx context.Context, ask Asker) (string, error) {
	if s.p.Phone == "" {
		return "", errors.New("ERR255: phone is required to log in")
	}
	if err := os.MkdirAll(filepath.Dir(s.p.Session), 0o700); err != nil {
		return "", fmt.Errorf("ERR256: %w", err)
	}
	c := s.client()
	var who string
	err := c.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(flowAuth{phone: s.p.Phone, ask: ask}, auth.SendCodeOptions{})
		if err := c.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("ERR257: login failed: %w", err)
		}
		u, err := c.Self(ctx)
		if err != nil {
			return fmt.Errorf("ERR258: %w", err)
		}
		who = userName(u)
		return nil
	})
	return who, err
}

// SessionPath is where the session of the adapter is kept.
func (s *Source) SessionPath() string { return s.p.Session }
