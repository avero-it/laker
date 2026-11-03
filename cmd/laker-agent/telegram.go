package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/avero-it/laker/intern/adapters/sources"
	"github.com/avero-it/laker/intern/adapters/sources/telegram"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/signaler"
)

func telegramCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "telegram", Short: "Telegram commands"}
	cmd.AddCommand(&cobra.Command{
		Use:   "login <adapter>",
		Short: "Log in to Telegram for a telegram adapter (the session stays on this machine)",
		Long: "Log in as the account of the adapter: Telegram sends a code to your Telegram apps, you type it here\n" +
			"(and your password, with two-step verification). The session is saved in the agent data directory,\n" +
			"readable by you only; it never leaves this machine. Telegram lists it among your active sessions\n" +
			"as \"laker-agent\", where you can end it.",
		Args: cobra.ExactArgs(1),
		Run:  func(cmd *cobra.Command, args []string) { commandTelegramLogin(*configPath, args[0]) },
	})
	return cmd
}

// termAsker reads the code and the password on the terminal.
type termAsker struct{ p *prompter }

func (a termAsker) Code() (string, error) {
	return a.p.ask("code sent to your Telegram apps: "), nil
}

func (a termAsker) Password() (string, error) {
	return a.p.secret("two-step verification password: "), nil
}

func commandTelegramLogin(configPath, name string) {
	cfg := mustConfig(configPath)
	for _, a := range cfg.Adapters {
		if a.Name != name {
			continue
		}
		if a.Type != telegram.Type {
			fatalf("ERR822: %s is a %s adapter, not telegram", name, a.Type)
		}
		// one logs in before enabling the adapter: expand its params anyway
		if err := config.ExpandParams(&a); err != nil {
			fatalf("ERR826: %v", err)
		}
		src, err := sources.Build(a, sources.Env{CommitEvery: 1, DataDir: cfg.Agent.DataDir, TmpDir: tmpDir(cfg)}, zerolog.Nop())
		if err != nil {
			fatalf("ERR823: %v", err)
		}
		tgs := src.(*telegram.Source)
		ctx, cancel := signaler.Context(context.Background(), zerolog.Nop())
		defer cancel()
		p := &prompter{in: bufio.NewReader(os.Stdin), terminal: term.IsTerminal(int(os.Stdin.Fd()))}
		who, err := tgs.Login(ctx, termAsker{p})
		if err != nil {
			fatalf("ERR824: %v", err)
		}
		fmt.Printf("logged in as %s; session in %s (keep it private: it gives access to the account)\n", who, tgs.SessionPath())
		fmt.Printf("enable the adapter, then: laker-agent config validate\n")
		return
	}
	fatalf("ERR825: no adapter named %q in the configuration", name)
}
