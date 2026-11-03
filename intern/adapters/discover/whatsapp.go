package discover

import (
	"context"
	"path/filepath"

	"github.com/avero-it/laker/pkg/config"
)

// whatsapp notices a WhatsApp desktop client and proposes a whatsapp adapter.
// It only checks that the data folder exists: it never reads the client's
// data (the agent pairs as a linked device of its own).
func whatsapp(_ context.Context, env Env, prefix string) []Candidate {
	var dirs []string
	switch env.GOOS {
	case "darwin":
		dirs = []string{
			filepath.Join(env.Home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared"),
			filepath.Join(env.Home, "Library", "Application Support", "WhatsApp"),
		}
	case "windows":
		dirs = []string{
			filepath.Join(env.Getenv("LOCALAPPDATA"), "Packages", "5319275A.WhatsAppDesktop_cv1g1gvanyjgm"),
			filepath.Join(env.Getenv("APPDATA"), "WhatsApp"),
		}
	default:
		dirs = []string{
			filepath.Join(env.Home, ".var", "app", "com.rtosta.zapzap"),
			filepath.Join(env.Home, ".var", "app", "com.github.eneshecan.WhatsAppForLinux"),
			filepath.Join(env.Home, ".config", "whatsapp-for-linux"),
		}
	}
	for _, d := range dirs {
		if st, err := env.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		n := name(prefix, "whatsapp", "")
		return []Candidate{{
			Key: "whatsapp",
			Adapter: config.GenAdapter{
				Name: n, Type: "whatsapp", Schedule: "0 0 * * * *",
				Comment: "A WhatsApp desktop client is installed (" + d + ").",
				Params: []config.GenParam{
					{Key: "chats", Value: []string{"**"}}, {Key: "exclude", Value: []string{}},
					{Key: "media", Value: true}, {Key: "mediamaxsize", Value: 52428800},
				},
			},
			Notes: []string{"pair it with your phone: laker-agent whatsapp login " + n +
				"; it is an unofficial linked device, and must run at least every few days or WhatsApp unlinks it"},
		}}
	}
	return nil
}
