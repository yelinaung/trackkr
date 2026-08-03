package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"
)

const argumentExtra = "extra"

func TestServeUntilShutdownWaitsForDrain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	httpServer := &blockingHTTPServer{
		shutdownStarted: make(chan struct{}),
		allowShutdown:   make(chan struct{}),
		listenerClosed:  make(chan struct{}),
	}
	result := make(chan error, 1)
	logger := zerolog.Nop()
	go func() {
		result <- serveUntilShutdown(ctx, httpServer, &logger)
	}()

	cancel()
	<-httpServer.shutdownStarted
	<-httpServer.listenerClosed
	select {
	case err := <-result:
		t.Fatalf("serve returned before shutdown drained handlers: %v", err)
	default:
	}

	close(httpServer.allowShutdown)
	if err := <-result; err != nil {
		t.Fatalf("serveUntilShutdown: %v", err)
	}
}

func TestServeUntilShutdownForceClosesAfterDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		httpServer := &timeoutHTTPServer{
			closed: make(chan struct{}),
		}
		result := make(chan error, 1)
		logger := zerolog.Nop()
		go func() {
			result <- serveUntilShutdown(ctx, httpServer, &logger)
		}()

		cancel()
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatalf("serveUntilShutdown: %v", err)
		}
		if !httpServer.wasClosed() {
			t.Fatal("server was not force-closed after the shutdown deadline")
		}
	})
}

func TestCommandFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "bare invocation serves", args: []string{serverBinary}, want: commandServe},
		{name: commandServe, args: []string{serverBinary, commandServe}, want: commandServe},
		{name: commandMigrate, args: []string{serverBinary, commandMigrate}, want: commandMigrate},
		{name: commandMigrationStatus, args: []string{serverBinary, commandMigrationStatus}, want: commandMigrationStatus},
		{name: commandMigrationForce, args: []string{serverBinary, commandMigrationForce, "1"}, want: commandMigrationForce},
		{name: commandVersion, args: []string{serverBinary, commandVersion}, want: commandVersion},
		{name: "create user", args: []string{serverBinary, commandCreateUser, "alice", "password"}, want: commandCreateUser},
		{name: "create device", args: []string{serverBinary, commandCreateDevice, "alice", "laptop"}, want: commandCreateDevice},
		{name: "unknown command", args: []string{serverBinary, "status"}, wantErr: true},
		{name: "serve arguments", args: []string{serverBinary, commandServe, argumentExtra}, wantErr: true},
		{name: "migrate arguments", args: []string{serverBinary, commandMigrate, argumentExtra}, wantErr: true},
		{name: "migration status arguments", args: []string{serverBinary, commandMigrationStatus, argumentExtra}, wantErr: true},
		{name: "migration force without version", args: []string{serverBinary, commandMigrationForce}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := commandFor(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("commandFor(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("commandFor(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestVersionReport(t *testing.T) {
	t.Parallel()

	const want = "version=dev commit=none build_date=unknown"
	if got := versionReport(); got != want {
		t.Errorf("versionReport() = %q, want %q", got, want)
	}
}

type blockingHTTPServer struct {
	shutdownStarted chan struct{}
	allowShutdown   chan struct{}
	listenerClosed  chan struct{}
}

func (s *blockingHTTPServer) ListenAndServe() error {
	<-s.shutdownStarted
	close(s.listenerClosed)
	return http.ErrServerClosed
}

func (s *blockingHTTPServer) Shutdown(context.Context) error {
	close(s.shutdownStarted)
	<-s.allowShutdown
	return nil
}

func (s *blockingHTTPServer) Close() error {
	return nil
}

type timeoutHTTPServer struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func (s *timeoutHTTPServer) ListenAndServe() error {
	<-s.closed
	return http.ErrServerClosed
}

func (s *timeoutHTTPServer) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *timeoutHTTPServer) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *timeoutHTTPServer) wasClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}
