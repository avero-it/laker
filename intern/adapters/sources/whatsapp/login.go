package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// Pairer shows the user how to link this device: a QR code to scan
// (WhatsApp > Linked devices > Link a device), or the code to type on the
// phone when pairing by phone number.
type Pairer interface {
	QR(code string)
	Code(code string)
}

// Login links the agent to the phone as a new device. With phone set
// (international format, digits only) it pairs by code, otherwise by QR. The
// session database (mode 0600) stays on this machine. What the phone sends
// right after pairing is kept in the spool for the first run.
func (s *Source) Login(ctx context.Context, phone string, pair Pairer) (string, error) {
	c, dev, err := s.openStore(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dev.ID != nil {
		c.Close()
		return s.paired(ctx)
	}
	sp, err := newSpool(s.p.Spool)
	if err != nil {
		return "", err
	}
	cli := s.client(dev)
	defer cli.Disconnect()

	connected := make(chan struct{}, 1)
	failed := make(chan error, 1)
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		switch e := evt.(type) {
		case *events.Message:
			kind, data, err := encode(e)
			if err != nil || kind == "" {
				return true
			}
			if err := sp.put(kind, data); err != nil {
				select {
				case failed <- err:
				default:
				}
				return false
			}
		case *events.Connected:
			select {
			case connected <- struct{}{}:
			default:
			}
		case *events.PairError:
			select {
			case failed <- fmt.Errorf("ERR27F: pairing failed: %w", e.Error):
			default:
			}
		}
		return true
	})
	// the reconnection right after pairing is what logs the device in
	cli.EnableAutoReconnect = true

	if phone == "" {
		qr, err := cli.GetQRChannel(ctx)
		if err != nil {
			return "", fmt.Errorf("ERR280: %w", err)
		}
		if err := cli.ConnectContext(ctx); err != nil {
			return "", fmt.Errorf("ERR281: cannot connect to WhatsApp: %w", err)
		}
		for item := range qr {
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				pair.QR(item.Code)
			case whatsmeow.QRChannelSuccess.Event:
			case whatsmeow.QRChannelEventError:
				return "", fmt.Errorf("ERR282: pairing failed: %w", item.Error)
			default:
				return "", fmt.Errorf("ERR283: pairing stopped: %s", item.Event)
			}
		}
	} else {
		if err := cli.ConnectContext(ctx); err != nil {
			return "", fmt.Errorf("ERR287: cannot connect to WhatsApp: %w", err)
		}
		code, err := cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome ("+deviceName+")")
		if err != nil {
			return "", fmt.Errorf("ERR284: %w", err)
		}
		pair.Code(code)
	}

	// paired: wait for the first login, which also publishes this device's
	// keys, then keep what arrives for a little while
	select {
	case <-connected:
	case err := <-failed:
		return "", err
	case <-time.After(3 * time.Minute):
		return "", errors.New("ERR285: the phone did not complete the pairing in time")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-time.After(20 * time.Second):
	case err := <-failed:
		return "", err
	case <-ctx.Done():
	}
	if cli.Store.ID == nil {
		return "", errors.New("ERR286: not paired")
	}
	return "+" + cli.Store.ID.User, nil
}
