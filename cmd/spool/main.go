// Command spool is the spool module's program: a door that posts each request
// dropped as a file in the inbox to the Hive's lead, as a thread of its own,
// and writes what comes of that thread to the outbox, one file per request.
//
//	spool         the module's program, as the daemon runs it
//	spool where   print where spool.json goes and the folders it names
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Hive-RSI-Lab/hive-spool/internal/hive"
	"github.com/Hive-RSI-Lab/hive-spool/internal/spool"
)

func main() {
	home := os.Getenv("HOME")
	cfgPath := filepath.Join(home, "spool.json")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "where":
		cfg, err := spool.LoadConfig(cfgPath)
		if err != nil {
			fail(err)
		}
		fmt.Printf("spool.json: %s\ninbox: %s\noutbox: %s\n", cfgPath, cfg.Inbox, cfg.Outbox)
		return
	case "", "serve":
	case "help", "-h", "--help":
		fmt.Println("usage: spool [where]; see docs/README.md")
		return
	default:
		fail(fmt.Errorf("unknown command %q; try help", cmd))
	}
	log.SetFlags(log.LstdFlags)
	h := &hive.Client{API: os.Getenv("HIVE_API"), Token: os.Getenv("HIVE_MODULE_TOKEN")}
	if h.API == "" {
		fail(fmt.Errorf("HIVE_API is not set: the Hive's daemon runs this program"))
	}
	name := os.Getenv("HIVE_MODULE")
	if name == "" {
		name = "spool"
	}
	cfg, err := spool.LoadConfig(cfgPath)
	if err != nil {
		h.Report(ctx, map[string]any{"kind": "error", "text": "spool: " + err.Error()})
		fail(err)
	}
	d := spool.New(cfg, h, name, home)
	d.Log = log.Printf
	// The Hive may still be starting: try again rather than die.
	for {
		err := d.Start(ctx)
		if err == nil {
			break
		}
		log.Printf("spool: starting: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	h.Report(ctx, map[string]any{"kind": "watching", "text": fmt.Sprintf("spool: taking requests from %s, answering in %s", cfg.Inbox, cfg.Outbox), "inbox": cfg.Inbox, "outbox": cfg.Outbox})
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "spool:", err)
	os.Exit(1)
}
