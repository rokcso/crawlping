// crawlping is a self-hosted IndexNow submitter.
//
// Sites deployed on Cloudflare call it from their own code; it forwards the
// URLs to api.indexnow.org from the machine it runs on, so the egress IP is
// yours rather than a shared platform address.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

const configEnv = "CRAWL_PING_CONFIG"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "healthcheck":
		err = cmdHealthcheck(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "crawlping:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, usageText())
}

func usageText() string {
	return fmt.Sprintf(`crawlping %s - submit URLs to IndexNow from your own machine

Usage:
  crawlping serve        [-config path]   run the HTTP API
  crawlping check        [-config path]   verify every configured key file
  crawlping healthcheck  [-config path]   exit 0 when the local API answers
  crawlping version

The config path defaults to $%s, then ./config.yaml.
`, version, configEnv)
}

func cmdServe(args []string) error {
	configPath := configFlag("serve", args)

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	srv := newServer(cfg, newIndexNowClient(cfg.IndexNowEndpoint), log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Addr is deliberately absent: the socket comes from net.Listen below, so
	// there is one place that decides what is bound and one place that can fail.
	httpServer := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind before any network check runs. net.Listen returns at once, so a port
	// that is already taken still fails loudly here instead of surfacing from a
	// goroutine, and the process is reachable as soon as this line is done.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	listenErr := make(chan error, 1)

	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	log.Info("listening", "addr", listener.Addr().String(), "sites", len(cfg.Sites), "version", version)

	// Key verification runs after the listener and in the background. Each check
	// may take up to 10s, so doing this first used to delay the first byte by
	// seconds-to-minutes on a slow-but-normal start. A bad key file is reported
	// and reflected in /health, but it does not stop the process: the origin may
	// simply be down, and restarting would not help.
	go srv.verifyAll(ctx)

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return httpServer.Shutdown(shutdownCtx)
}

func cmdCheck(args []string) error {
	configPath := configFlag("check", args)

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	client := newIndexNowClient(cfg.IndexNowEndpoint)

	failed := 0
	unknown := 0

	for _, site := range cfg.Sites {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := client.verifyKey(ctx, site)
		cancel()

		switch keyStateAfterCheck(err) {
		case keyStateVerified:
			fmt.Printf("ok    %-32s %s\n", site.Host, site.KeyLocation)
		case keyStateFailed:
			failed++
			fmt.Printf("FAIL  %-32s %s\n      %v\n", site.Host, site.KeyLocation, err)
		default:
			// The file was never read, so nothing is known. Reporting this as a
			// failure would blame a key file for an origin that is simply down.
			unknown++
			fmt.Printf("????  %-32s %s\n      could not determine: %v\n", site.Host, site.KeyLocation, err)
		}
	}

	switch {
	case failed > 0 && unknown > 0:
		return fmt.Errorf("%d of %d key files failed, %d could not be checked", failed, len(cfg.Sites), unknown)
	case failed > 0:
		return fmt.Errorf("%d of %d key files failed", failed, len(cfg.Sites))
	case unknown > 0:
		return fmt.Errorf("%d of %d key files could not be checked", unknown, len(cfg.Sites))
	}

	return nil
}

func cmdHealthcheck(args []string) error {
	configPath := configFlag("healthcheck", args)

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(localHealthURL(cfg.Listen))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}

	return nil
}

func configFlag(command string, args []string) string {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	path := flags.String("config", defaultConfigPath(), "path to config.yaml")
	_ = flags.Parse(args)
	return *path
}

func defaultConfigPath() string {
	if path := os.Getenv(configEnv); path != "" {
		return path
	}
	return "config.yaml"
}

// localHealthURL turns the listen address into something the healthcheck
// subcommand can dial from inside the same container.
func localHealthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080/health"
	}

	switch host {
	// net.SplitHostPort strips the brackets, so an IPv6 wildcard listen address
	// arrives here as "::" and only that form needs handling.
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port) + "/health"
}
