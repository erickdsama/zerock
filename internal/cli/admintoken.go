package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/server"
	"github.com/erickdsama/zerock/internal/store"
)

const usageAdminToken = `Mint an admin token directly from the server's database.

Tokens are stored only as a hash, so a lost one cannot be read back. This is the
way in when the first-run token is gone from the journal: it writes a new admin
token straight to the database, without needing an existing one.

The database is locked by the running server, so stop it first:

Usage:
  sudo systemctl stop zerock
  sudo zerock admin-token
  sudo systemctl start zerock

Flags:
  --config path   server config, for its data_dir (default /etc/zerock/zerock.yaml)
  --label name    label for the new token (default "recovered-admin")
  --list          show existing tokens instead of creating one
`

func runAdminToken(_ context.Context, args []string) error {
	fs := newFlagSet("admin-token", usageAdminToken)
	configPath := fs.String("config", defaultServerConfigPath(), "server config")
	label := fs.String("label", "recovered-admin", "label for the new token")
	list := fs.Bool("list", false, "list existing tokens instead of creating one")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := server.LoadConfigForInspection(*configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config at %s\n  %s", *configPath,
				dim("pass --config if the server keeps its config elsewhere"))
		}
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot read %s\n  %s", *configPath, dim("re-run with sudo"))
		}
		return err
	}

	dbPath := filepath.Join(cfg.DataDir, "zerock.db")
	db, err := store.Open(dbPath)
	if err != nil {
		// bbolt takes an exclusive lock, so a running server makes this fail.
		// That is the common case and deserves the obvious instruction.
		if strings.Contains(err.Error(), "timeout") {
			return fmt.Errorf("the database at %s is locked by the running server\n  %s",
				dbPath, dim("sudo systemctl stop zerock, then run this again"))
		}
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot open %s\n  %s", dbPath, dim("re-run with sudo"))
		}
		return err
	}
	defer db.Close()

	if *list {
		tokens, err := db.ListTokens()
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			fmt.Println("No tokens in the database.")
			return nil
		}
		tw := newTable()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", dim("ID"), dim("LABEL"), dim("SCOPES"), dim("STATUS"))
		for _, t := range tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				t.ID, t.Label, strings.Join(t.Scopes, ","), statusWord(t.Status(time.Now().UTC())))
		}
		return tw.Flush()
	}

	tok, secret, err := db.CreateToken(store.CreateTokenOpts{
		Label:  *label,
		Scopes: []string{store.ScopeAdmin, store.ScopeTunnel},
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s created admin token %s\n\n", green("✓"), bold(tok.ID))
	fmt.Printf("  %s\n\n", bold(secret))
	fmt.Printf("  %s\n", amber("Shown once only. Copy it now."))
	fmt.Printf("  %s\n", dim(fmt.Sprintf("zerock login --server %s --token %s", cfg.APIHost, secret)))
	fmt.Printf("\n%s\n", dim("start the server again: sudo systemctl start zerock"))
	return nil
}
