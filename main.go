package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type providerConfig struct {
	Name      string           `json:"name"`
	Endpoints []endpointConfig `json:"endpoints"`
}

type endpointConfig struct {
	Method    string           `json:"method"`
	Path      string           `json:"path"`
	Policy    string           `json:"policy"`
	Repeat    bool             `json:"repeat"`
	Responses []responseConfig `json:"responses"`
}

type responseConfig struct {
	Name    string            `json:"name"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
	Delay   int64             `json:"delay"`
	Weight  int64             `json:"weight"`
	delay   time.Duration
}

// Each route owns its sequence counter. Concurrent requests select outcomes
// atomically; delays happen after selection so they don't block other requests.
type endpoint struct {
	config endpointConfig
	total  int64
	mu     sync.Mutex
	next   int
}

func newEndpoint(config endpointConfig) (*endpoint, error) {
	if config.Method == "" || strings.ContainsAny(config.Method, " \t\r\n") {
		return nil, errors.New("method must be a nonempty HTTP method")
	}
	if !strings.HasPrefix(config.Path, "/") {
		return nil, errors.New("path must start with /")
	}
	if config.Policy == "" {
		config.Policy = "random"
	}
	if config.Policy != "random" && config.Policy != "sequence" {
		return nil, errors.New("policy must be random or sequence")
	}
	if config.Repeat && config.Policy != "sequence" {
		return nil, errors.New("repeat requires sequence policy")
	}
	if len(config.Responses) == 0 {
		return nil, errors.New("at least one response is required")
	}
	e := &endpoint{config: config}
	for i := range e.config.Responses {
		r := &e.config.Responses[i]
		if r.Status < 200 || r.Status > 599 {
			return nil, fmt.Errorf("response %d: status must be between 200 and 599", i+1)
		}
		if (r.Status == http.StatusNoContent || r.Status == http.StatusNotModified) && len(r.Body) != 0 {
			return nil, fmt.Errorf("response %d: status %d cannot have a body", i+1, r.Status)
		}
		if r.Delay < 0 || r.Delay > int64((1<<63-1)/time.Millisecond) {
			return nil, fmt.Errorf("response %d: delay must be a nonnegative number of milliseconds no greater than %d", i+1, int64((1<<63-1)/time.Millisecond))
		}
		r.delay = time.Duration(r.Delay) * time.Millisecond
		if r.Weight < 0 {
			return nil, fmt.Errorf("response %d: weight cannot be negative", i+1)
		}
		if r.Weight == 0 {
			r.Weight = 1
		}
		const maxInt64 = int64(1<<63 - 1)
		if r.Weight > maxInt64-e.total {
			return nil, errors.New("sum of response weights is too large")
		}
		e.total += r.Weight
	}
	return e, nil
}

func (e *endpoint) selectResponse() responseConfig {
	if e.config.Policy == "sequence" {
		e.mu.Lock()
		defer e.mu.Unlock()

		r := e.config.Responses[e.next]
		if e.next < len(e.config.Responses)-1 {
			e.next++
		} else if e.config.Repeat {
			e.next = 0
		}

		return r
	}

	selection := rand.Int63n(e.total)

	for _, r := range e.config.Responses {
		if selection < r.Weight {
			return r
		}

		selection -= r.Weight
	}

	panic("validated response weights exhausted")
}

func (e *endpoint) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r := e.selectResponse()
	started := time.Now()

	if r.delay > 0 {
		timer := time.NewTimer(r.delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-req.Context().Done():
			slog.Info("request canceled", "method", req.Method, "path", req.URL.Path, "response", r.Name, "elapsed", time.Since(started))
			return
		}
	}

	if len(r.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}

	for name, value := range r.Headers {
		w.Header().Set(name, value)
	}

	w.WriteHeader(r.Status)
	if len(r.Body) > 0 && req.Method != http.MethodHead {
		if _, err := w.Write(r.Body); err != nil {
			slog.Error("response write failed", "method", req.Method, "path", req.URL.Path, "response", r.Name, "error", err)
			return
		}
	}

	slog.Info("request completed", "method", req.Method, "path", req.URL.Path, "response", r.Name, "status", r.Status, "elapsed", time.Since(started))
}

func loadHandler(dir string) (http.Handler, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no provider .json files found in %q", dir)
	}

	mux := http.NewServeMux()
	for _, file := range files {
		provider, err := readProvider(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}

		if provider.Name == "" || len(provider.Endpoints) == 0 {
			return nil, fmt.Errorf("%s: name and at least one endpoint are required", file)
		}

		for _, config := range provider.Endpoints {
			e, err := newEndpoint(config)
			if err != nil {
				return nil, fmt.Errorf("%s: %s %s: %w", file, config.Method, config.Path, err)
			}

			if err := registerEndpoint(mux, config.Method+" "+config.Path, e); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}

			slog.Info("registered endpoint", "provider", provider.Name, "route", config.Method+" "+config.Path, "policy", e.config.Policy)
		}
	}

	return mux, nil
}

// ServeMux reports malformed or conflicting patterns by panicking. Convert
// those configuration errors into normal startup errors.
func registerEndpoint(mux *http.ServeMux, pattern string, e *endpoint) (err error) {
	defer func() {
		if problem := recover(); problem != nil {
			err = fmt.Errorf("invalid or conflicting route %q: %v", pattern, problem)
		}
	}()

	mux.Handle(pattern, e)
	return nil
}

func readProvider(path string) (providerConfig, error) {
	var config providerConfig
	f, err := os.Open(path)
	if err != nil {
		return config, err
	}

	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return config, errors.New("expected exactly one JSON object")
	}

	return config, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	configDir := flag.String("config", "providers", "directory containing provider JSON files")
	flag.Parse()
	handler, err := loadHandler(*configDir)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: scenarios may deliberately delay responses.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
			server.Close()
		}
	}()

	slog.Info("listening", "address", *addr)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}

	<-stopped
}
