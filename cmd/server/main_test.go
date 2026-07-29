package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"
)

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
