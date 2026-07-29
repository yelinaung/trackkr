package main

import (
	"context"
	"net/http"
	"testing"

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
