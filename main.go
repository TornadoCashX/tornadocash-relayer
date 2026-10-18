package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// errWorkersStillActive marks a shutdown where workers outlived the deadline.
// Resources are deliberately left open in that case: closing SQLite under an
// active worker could interrupt a state transition. SQLite WAL with
// synchronous=FULL keeps committed state durable, and process exit releases
// the file lock.
var errWorkersStillActive = errors.New("active jobs did not stop before shutdown deadline")

// HTTP server limits and shutdown deadlines.
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 15 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
	httpShutdownTimeout   = 15 * time.Second
	workerDrainTimeout    = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, "Usage:", startupUsage)
			return
		}
		logErrorf("%v", err)
		os.Exit(1)
	}
}

// run owns every resource so deferred cleanup always executes before main
// chooses the exit code.
func run(args []string) error {
	// Before any secret (.env) or request data is in memory.
	if err := hardenProcess(); err != nil {
		return fmt.Errorf("process hardening failed: %w", err)
	}
	cfg, err := loadStartupConfig(args)
	if err != nil {
		return err
	}
	relayer, err := NewRelayer(cfg)
	if err != nil {
		return fmt.Errorf("relayer init error: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, listener, err := listenHTTP(cfg, relayer)
	if err != nil {
		_ = relayer.Close()
		return err
	}
	logStartupBanner(cfg, server.Addr)
	serverErr := startServing(relayer, server, listener)
	runErr := waitForShutdownTrigger(ctx, serverErr)
	return shutdown(relayer, server, runErr)
}

// loadStartupConfig parses the command line, loads the required .env file and
// then the JSON/environment configuration. flag.ErrHelp is returned unwrapped
// so main can print the usage and exit 0.
func loadStartupConfig(args []string) (Config, error) {
	opts, err := parseStartupOptions(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, err
		}
		return Config{}, fmt.Errorf("usage: %s: %w", startupUsage, err)
	}
	if err := loadDotEnvFileRequired(opts.envPath); err != nil {
		return Config{}, fmt.Errorf("environment file error: %w", err)
	}
	cfg, err := LoadConfig(opts.configPath)
	if err != nil {
		return Config{}, fmt.Errorf("config error: %w", err)
	}
	return cfg, nil
}

// listenHTTP builds the HTTP server and binds its listener. The caller closes
// the relayer when binding fails.
func listenHTTP(cfg Config, relayer *Relayer) (*http.Server, net.Listener, error) {
	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, cfg.Port),
		Handler:           relayer.routes(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("http listen failed: %w", err)
	}
	return server, listener, nil
}

// logStartupBanner logs the listening address and the public configuration.
func logStartupBanner(cfg Config, addr string) {
	tokens := strings.Join(orderedKeys(cfg.Instances, tokenOrder), ",")
	logInfof(
		"Go withdraw relayer listening on %s, version=%s, netId=%d, tokens=%s, ozRelayer=%s, rewardAccount=%s, target=pool",
		addr,
		cfg.Version,
		cfg.ChainID.Uint64(),
		tokens,
		cfg.OZRelayerID,
		cfg.RewardAccount.Hex(),
	)
}

// startServing serves HTTP and starts the startup sequence. It listens first
// so probes can observe "not ready" during startup instead of a closed port;
// admission stays closed until OZ is verified. The returned channel receives
// the result of Serve.
func startServing(relayer *Relayer, server *http.Server, listener net.Listener) <-chan error {
	relayer.startupPending.Store(true)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()
	runtimeCtx := relayer.runtimeContext()
	relayer.workerWG.Add(1)
	go func() {
		defer relayer.workerWG.Done()
		relayer.runStartup(runtimeCtx)
	}()
	return serverErr
}

// waitForShutdownTrigger blocks until a shutdown signal arrives or the HTTP
// server stops; it returns the server failure, if any.
func waitForShutdownTrigger(ctx context.Context, serverErr <-chan error) error {
	select {
	case <-ctx.Done():
		logInfof("shutdown signal received")
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server failed: %w", err)
		}
	}
	return nil
}

// shutdown closes admission, drains HTTP handlers and workers, and closes
// resources only once every worker has stopped (see errWorkersStillActive).
func shutdown(relayer *Relayer, server *http.Server, runErr error) error {
	// Close admission before waiting for handlers, and prevent handlers already
	// in flight from starting new workers while shutdown drains.
	relayer.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logErrorf("http shutdown failed: %v", err)
		_ = server.Close()
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), workerDrainTimeout)
	defer waitCancel()
	if err := relayer.Wait(waitCtx); err != nil {
		return errors.Join(runErr, fmt.Errorf("%w; refusing to close resources while workers are active: %v", errWorkersStillActive, err))
	}
	if err := relayer.Close(); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("relayer close failed: %w", err))
	}
	return runErr
}
