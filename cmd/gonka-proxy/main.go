package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/glaicer/gonka-proxy/internal/admin"
	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

const shutdownTimeout = 10 * time.Second

// Environment variables. Tokens live in the environment rather than the
// config file so a pushed config can never lock the operator out.
const (
	envClientToken        = "GONKA_PROXY_CLIENT_TOKEN"
	envAdminToken         = "GONKA_PROXY_ADMIN_TOKEN"
	envAdminListenAddress = "GONKA_PROXY_ADMIN_LISTEN_ADDRESS"
	envAdminPersist       = "GONKA_PROXY_ADMIN_PERSIST"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	logger := log.New(os.Stdout, "", log.LstdFlags)
	if err := runWith(ctx, *configPath, logger, runOptions{getenv: os.Getenv, reload: reload}); err != nil {
		logger.Printf("error=%v", err)
		os.Exit(1)
	}
}

// runOptions carries process inputs so tests can inject them.
type runOptions struct {
	getenv func(string) string
	// reload delivers SIGHUP: reload the config file.
	reload <-chan os.Signal
	// ready, when set, receives the bound proxy and admin addresses.
	ready func(proxyAddress, adminAddress string)
}

func run(ctx context.Context, configPath string, logger proxy.Logger) error {
	return runWith(ctx, configPath, logger, runOptions{getenv: os.Getenv})
}

func runWith(ctx context.Context, configPath string, logger proxy.Logger, options runOptions) error {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	getenv := options.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	handler, err := proxy.NewWithOptions(cfg, proxy.Options{
		Logger:      logger,
		ClientToken: getenv(envClientToken),
	})
	if err != nil {
		return fmt.Errorf("create proxy: %w", err)
	}

	var adminHandler http.Handler
	adminAddress := strings.TrimSpace(getenv(envAdminListenAddress))
	if token := strings.TrimSpace(getenv(envAdminToken)); token != "" {
		persist, err := parseBoolEnv(envAdminPersist, getenv(envAdminPersist))
		if err != nil {
			return err
		}
		adminHandler, err = admin.New(handler, admin.Options{
			Token:      token,
			ConfigPath: configPath,
			Persist:    persist,
			Logger:     logger,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", envAdminToken, err)
		}
	} else if adminAddress != "" {
		return fmt.Errorf("%s is set but %s is not", envAdminListenAddress, envAdminToken)
	}

	serverContext, cancelServerContext := context.WithCancel(ctx)
	defer cancelServerContext()
	requests := newRequestTracker()

	var mainHandler http.Handler = requests.wrap(handler)
	if adminHandler != nil && adminAddress == "" {
		mainHandler = withAdmin(mainHandler, adminHandler)
	}
	servers := []*namedServer{{
		name:    "proxy",
		address: cfg.ListenAddress,
		server: &http.Server{
			Handler:           mainHandler,
			ReadHeaderTimeout: 30 * time.Second,
			BaseContext: func(net.Listener) context.Context {
				return serverContext
			},
		},
	}}
	if adminHandler != nil && adminAddress != "" {
		servers = append(servers, &namedServer{
			name:    "admin",
			address: adminAddress,
			server: &http.Server{
				Handler:           withAdmin(http.NotFoundHandler(), adminHandler),
				ReadHeaderTimeout: 30 * time.Second,
			},
		})
	}

	for _, entry := range servers {
		listener, err := net.Listen("tcp", entry.address)
		if err != nil {
			for _, started := range servers {
				if started.listener != nil {
					_ = started.listener.Close()
				}
			}
			return fmt.Errorf("listen on %s: %w", entry.address, err)
		}
		entry.listener = listener
	}
	logger.Printf("listening on %s", cfg.ListenAddress)
	switch {
	case adminHandler == nil:
		logger.Printf("admin API disabled (%s not set)", envAdminToken)
	case adminAddress != "":
		logger.Printf("admin API listening on %s", adminAddress)
	default:
		logger.Printf("admin API enabled on %s/admin/", cfg.ListenAddress)
	}
	if options.ready != nil {
		adminBound := ""
		if len(servers) > 1 {
			adminBound = servers[1].listener.Addr().String()
		}
		options.ready(servers[0].listener.Addr().String(), adminBound)
	}

	serveResult := make(chan error, len(servers))
	for _, entry := range servers {
		entry := entry
		go func() {
			err := entry.server.Serve(entry.listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				err = fmt.Errorf("serve %s: %w", entry.name, err)
			} else {
				err = nil
			}
			serveResult <- err
		}()
	}

	shutdown := func() error {
		requests.cancelAll()
		cancelServerContext()
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var firstErr error
		for _, entry := range servers {
			if err := entry.server.Shutdown(shutdownContext); err != nil {
				_ = entry.server.Close()
				if firstErr == nil {
					firstErr = fmt.Errorf("shutdown: %w", err)
				}
			}
		}
		return firstErr
	}

	for {
		select {
		case err := <-serveResult:
			if err != nil {
				_ = shutdown()
				return err
			}
			if err := shutdown(); err != nil {
				return err
			}
			return nil
		case <-options.reload:
			reloadFromFile(handler, configPath, logger)
		case <-ctx.Done():
			logger.Printf("shutdown requested")
			if err := shutdown(); err != nil {
				return err
			}
			for range servers {
				if err := <-serveResult; err != nil {
					return fmt.Errorf("serve after shutdown: %w", err)
				}
			}
			logger.Printf("shutdown complete")
			return nil
		}
	}
}

type namedServer struct {
	name     string
	address  string
	server   *http.Server
	listener net.Listener
}

// reloadFromFile handles SIGHUP: re-read the config file and swap it in, or
// keep the running configuration when the file is invalid.
func reloadFromFile(handler *proxy.Server, configPath string, logger proxy.Logger) {
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Printf("config reload failed - keeping the running configuration - %v", err)
		return
	}
	result, err := handler.Reload(cfg, nil)
	if err != nil {
		logger.Printf("config reload failed - keeping the running configuration - %v", err)
		return
	}
	if !result.Changed {
		logger.Printf("config reload - unchanged - version=%s", shortVersion(result.Version))
	}
}

func shortVersion(version string) string {
	if len(version) > 12 {
		return version[:12]
	}
	return version
}

// withAdmin routes /admin and /admin/* to the admin handler.
func withAdmin(next, adminHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
			adminHandler.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseBoolEnv(name, value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

type activeRequest struct {
	cancel context.CancelFunc
}

type requestTracker struct {
	mu       sync.Mutex
	active   map[*activeRequest]struct{}
	canceled bool
}

func newRequestTracker() *requestTracker {
	return &requestTracker{active: make(map[*activeRequest]struct{})}
}

func (t *requestTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestContext, cancel := context.WithCancel(r.Context())
		request := &activeRequest{cancel: cancel}
		t.add(request)
		defer func() {
			t.remove(request)
			cancel()
		}()
		next.ServeHTTP(w, r.WithContext(requestContext))
	})
}

func (t *requestTracker) add(request *activeRequest) {
	t.mu.Lock()
	if t.canceled {
		t.mu.Unlock()
		request.cancel()
		return
	}
	t.active[request] = struct{}{}
	t.mu.Unlock()
}

func (t *requestTracker) remove(request *activeRequest) {
	t.mu.Lock()
	delete(t.active, request)
	t.mu.Unlock()
}

func (t *requestTracker) cancelAll() {
	t.mu.Lock()
	t.canceled = true
	active := make([]*activeRequest, 0, len(t.active))
	for request := range t.active {
		active = append(active, request)
	}
	t.mu.Unlock()

	for _, request := range active {
		request.cancel()
	}
}
