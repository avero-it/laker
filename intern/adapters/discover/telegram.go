package discover

import (
	"context"
	"path/filepath"

	"github.com/avero-it/laker/pkg/config"
)

// telegram notices Telegram Desktop and proposes a telegram adapter. It only
// checks that the data folder exists: it never reads Telegram Desktop's
// session (the agent logs in on its own).
func telegram(_ context.Context, env Env, prefix string) []Candidate {
	var dirs []string
	switch env.GOOS {
	case "darwin":
		dirs = []string{filepath.Join(env.Home, "Library", "Application Support", "Telegram Desktop", "tdata")}
	case "windows":
		dirs = []string{filepath.Join(env.Getenv("APPDATA"), "Telegram Desktop", "tdata")}
	default:
		dirs = []string{
			filepath.Join(env.Home, ".local", "share", "TelegramDesktop", "tdata"),
			filepath.Join(env.Home, "snap", "telegram-desktop", "current", ".local", "share", "TelegramDesktop", "tdata"),
			filepath.Join(env.Home, ".var", "app", "org.telegram.desktop", "data", "TelegramDesktop", "tdata"),
		}
	}
	for _, d := range dirs {
		if st, err := env.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		n := name(prefix, "telegram", "")
		return []Candidate{{
			Key: "telegram",
			Adapter: config.GenAdapter{
				Name: n, Type: "telegram", Schedule: "0 */30 * * * *",
				Comment: "Telegram Desktop is installed (" + d + ").",
				Params: []config.GenParam{
					{Key: "apiid", Value: "${" + varName(n, "API_ID") + "}"},
					{Key: "apihash", Value: "${" + varName(n, "API_HASH") + "}"},
					{Key: "phone", Value: "", Comment: "the account, international format"},
					{Key: "chats", Value: []string{"**"}}, {Key: "exclude", Value: []string{}},
					{Key: "media", Value: true}, {Key: "mediamaxsize", Value: 52428800},
				},
			},
			Secrets: []Secret{
				{Var: varName(n, "API_ID"), Origin: "https://my.telegram.org, API development tools"},
				{Var: varName(n, "API_HASH"), Origin: "https://my.telegram.org, API development tools"},
			},
			Notes: []string{"set phone, then log in with: laker-agent telegram login " + n +
				"; the first import is an export that Telegram may delay about 24 hours"},
		}}
	}
	return nil
}
