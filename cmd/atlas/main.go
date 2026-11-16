package main

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/httpapi"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
func run() error {
	addr := os.Getenv("ATLAS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8763"
	}
	path := os.Getenv("ATLAS_STATE")
	if path == "" {
		path = "var/atlas-state.json"
	}
	st, e := persist.Open(path)
	if e != nil {
		return e
	}
	defer st.Close()
	svc := service.New(st)
	if raw := os.Getenv("ATLAS_RETENTION_PREVIEW_TTL"); raw != "" {
		ttl, e := time.ParseDuration(raw)
		if e != nil || ttl < time.Second || ttl > 24*time.Hour {
			return fmt.Errorf("ATLAS_RETENTION_PREVIEW_TTL must be a duration between 1s and 24h")
		}
		svc.PreviewTTL = ttl
	}
	worker := engine.New(st)
	if e = worker.Recover(); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	srv := &http.Server{Addr: addr, Handler: (httpapi.API{Service: svc, Worker: worker}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errc := make(chan error, 1)
	go func() { log.Printf("Atlas Render listening on %s", addr); errc <- srv.ListenAndServe() }()
	select {
	case e = <-errc:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
