package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/avero-it/laker/intern/adapters/sources"
	"github.com/avero-it/laker/intern/adapters/sources/whatsapp"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/signaler"
)

func whatsappCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "whatsapp", Short: "WhatsApp commands"}
	var phone string
	login := &cobra.Command{
		Use:   "login <adapter>",
		Short: "Pair a whatsapp adapter with your phone as a linked device (the session stays on this machine)",
		Long: "Link the agent to your phone like WhatsApp Web: scan the QR code shown here from WhatsApp >\n" +
			"Linked devices > Link a device, or with --phone type the code shown here on the phone. The phone\n" +
			"then sends the history, which the first run of the adapter stores. The session is saved in the\n" +
			"agent data directory, readable by you only; it never leaves this machine. The phone lists it as\n" +
			"\"laker-agent\", where you can unlink it. WhatsApp unlinks a device that stays offline for about\n" +
			"two weeks: schedule the adapter at least every few days.",
		Args: cobra.ExactArgs(1),
		Run:  func(cmd *cobra.Command, args []string) { commandWhatsappLogin(*configPath, args[0], phone) },
	}
	login.Flags().StringVar(&phone, "phone", "", "pair by code instead of QR: your number in international format")
	cmd.AddCommand(login)
	return cmd
}

type termPairer struct{}

func (termPairer) QR(code string) {
	fmt.Println("scan with WhatsApp > Linked devices > Link a device:")
	qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stdout)
}

func (termPairer) Code(code string) {
	fmt.Printf("on the phone: WhatsApp > Linked devices > Link a device > Link with phone number, then type %s\n", code)
}

func commandWhatsappLogin(configPath, name, phone string) {
	cfg := mustConfig(configPath)
	for _, a := range cfg.Adapters {
		if a.Name != name {
			continue
		}
		if a.Type != whatsapp.Type {
			fatalf("ERR827: %s is a %s adapter, not whatsapp", name, a.Type)
		}
		if err := config.ExpandParams(&a); err != nil {
			fatalf("ERR828: %v", err)
		}
		src, err := sources.Build(a, sources.Env{CommitEvery: 1, DataDir: cfg.Agent.DataDir, TmpDir: tmpDir(cfg)}, zerolog.Nop())
		if err != nil {
			fatalf("ERR829: %v", err)
		}
		was := src.(*whatsapp.Source)
		ctx, cancel := signaler.Context(context.Background(), zerolog.Nop())
		defer cancel()
		phone = strings.TrimLeft(strings.NewReplacer(" ", "", "-", "").Replace(phone), "+")
		who, err := was.Login(ctx, phone, termPairer{})
		if err != nil {
			fatalf("ERR82A: %v", err)
		}
		fmt.Printf("paired as %s; session in %s (keep it private: it gives access to the account)\n", who, was.SessionPath())
		fmt.Printf("enable the adapter, then: laker-agent config validate\n")
		return
	}
	fatalf("ERR82B: no adapter named %q in the configuration", name)
}
