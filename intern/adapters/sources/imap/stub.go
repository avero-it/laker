package imap

import "github.com/avero-it/laker/pkg/config"

// Stub is a documented, disabled imap adapter for `config generate`.
func Stub() config.GenAdapter {
	return config.GenAdapter{
		Name: "mail", Type: Type, Schedule: "@every 15m",
		Comment: "A mailbox over IMAP: every message is stored once as .eml, searchable.",
		Params: []config.GenParam{
			{Key: "host", Value: "imap.example.com"},
			{Key: "port", Value: 993},
			{Key: "security", Value: "tls", Comment: "tls (993) | starttls (143) | none"},
			{Key: "username", Value: "me@example.com"},
			{Key: "password", Value: "${LAKER_MAIL_PASSWORD}", Comment: "an app password where the provider asks for one (Gmail, Outlook)"},
			{Key: "folders", Value: []string{"**"}, Comment: `patterns; for Gmail only ["[Gmail]/All Mail"], to skip the label copies`},
			{Key: "exclude", Value: []string{"Trash", "Spam", "Junk"}},
			{Key: "since", Value: "", Comment: "YYYY-MM-DD limits the first run; empty reads everything"},
			{Key: "maxsize", Value: 104857600, Comment: "bytes; larger messages are recorded, not stored"},
		},
	}
}
