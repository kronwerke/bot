// kronwerke-bot is the Discord bot of the Kronwerke Minecraft server.
//
//	kronwerke-bot            run (configuration from the environment)
//	kronwerke-bot version    print the version and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kronwerke/bot/internal/bot"
	"github.com/kronwerke/bot/internal/update"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q. Run without arguments, or with \"version\".\n", os.Args[1])
			os.Exit(2)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	b, err := bot.New(cfg, log)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version)
	err = b.Run(ctx)
	switch {
	case errors.Is(err, bot.ErrRestart):
		log.Info("exiting for restart")
		os.Exit(0) // systemd restarts the unit (Restart=always)
	case errors.Is(err, context.Canceled):
		log.Info("stopped")
	case err != nil:
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// token accepts the bare token and the "Bot <token>" form the Authorization header uses.
func token(v string) string {
	v = strings.TrimSpace(v)
	return strings.TrimSpace(strings.TrimPrefix(v, "Bot "))
}

func config() (bot.Config, error) {
	stateDir := env("KW_STATE_DIR", "/srv/projects/kronwerke-bot/state")
	cfg := bot.Config{
		Token:         token(os.Getenv("DISCORD_TOKEN")),
		StatePath:     filepath.Join(stateDir, "state.json"),
		HTTPAddr:      env("KW_HTTP_ADDR", "127.0.0.1:13030"),
		MinecraftAddr: os.Getenv("KW_MC_ADDR"),
		RCONAddr:      os.Getenv("KW_RCON_ADDR"),
		RCONPassword:  os.Getenv("KW_RCON_PASSWORD"),
		Version:       version,
		APIBase:       os.Getenv("KW_DISCORD_API"), // tests only
	}
	if cfg.RCONAddr != "" && cfg.RCONPassword == "" {
		return cfg, errors.New("KW_RCON_ADDR is set but KW_RCON_PASSWORD is empty")
	}
	if repo := env("KW_UPDATE_REPO", "kronwerke/bot"); repo != "" && repo != "off" && version != "dev" {
		exe, err := os.Executable()
		if err != nil {
			return cfg, fmt.Errorf("find own binary: %w", err)
		}
		exe, _ = filepath.EvalSymlinks(exe)
		cfg.Updater = &update.Updater{
			Repo:      repo,
			API:       env("KW_UPDATE_API", "https://api.github.com"),
			Binary:    exe,
			TrialFile: filepath.Join(stateDir, "update.trial"),
			SkipFile:  filepath.Join(stateDir, "update.skip"),
			Current:   version,
		}
	}
	return cfg, nil
}
