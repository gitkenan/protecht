// Command agent is a Windows activity client. It polls the foreground
// window and, when that window changes, uploads the observation as hashed
// JSON evidence and submits an application.foreground event.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"protecht/internal/core"
)

func main() {
	opt, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if err := run(opt, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

type options struct {
	server   string
	name     string
	interval time.Duration
	config   string
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "http://127.0.0.1:8080", "protecht server base URL")
	name := fs.String("name", defaultDeviceName(), "device name used on first registration")
	interval := fs.Duration("interval", 3*time.Second, "how often to poll the foreground window")
	configPath := fs.String("config", "agent.json", "path to the local device id and token file")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return options{
		server:   *server,
		name:     *name,
		interval: *interval,
		config:   *configPath,
	}, nil
}

func defaultDeviceName() string {
	host, err := os.Hostname()
	if err != nil {
		return "win-agent"
	}
	return sanitizeDeviceName(host)
}

func sanitizeDeviceName(name string) string {
	name = strings.TrimSpace(name)
	if err := core.ValidateDeviceName(name); err != nil {
		return "win-agent"
	}
	return name
}

func validateFlags(opt options) error {
	if opt.interval <= 0 {
		return errors.New("interval must be positive")
	}
	if err := core.ValidateDeviceName(opt.name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if strings.TrimSpace(opt.server) == "" {
		return errors.New("server URL is required")
	}
	if strings.TrimSpace(opt.config) == "" {
		return errors.New("config path is required")
	}
	return nil
}

func run(opt options, out io.Writer) error {
	if err := validateFlags(opt); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return errors.New("foreground capture requires windows")
	}
	base, err := normalizeBase(opt.server)
	if err != nil {
		return err
	}
	c := newClient(base, "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, reused, err := ensureDevice(opt.config, opt.name, func(name string) (*core.Device, error) {
		return c.register(ctx, name)
	})
	if err != nil {
		return err
	}
	c.token = cfg.Token
	if reused && cfg.Name != opt.name {
		log.Printf("config %s is registered as %q; ignoring -name %q", opt.config, cfg.Name, opt.name)
	}
	if reused {
		log.Printf("reusing device %s (%s)", cfg.Name, cfg.ID)
	} else {
		log.Printf("registered device %s (%s); credentials saved to %s", cfg.Name, cfg.ID, opt.config)
	}

	return loop(ctx, opt.interval, time.Now, captureForeground, func(obs Observation, eventID, evidenceID string) (string, error) {
		return c.publish(ctx, obs, eventID, evidenceID)
	}, out)
}
