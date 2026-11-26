package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"desd-api/internal/config"
	"desd-api/internal/db"
	"desd-api/internal/hotfolder"
	"desd-api/internal/httpapi"
	"desd-api/internal/queue"
)

func runHealthcheck(addr string) int {
	url := "http://127.0.0.1" + addr + "/health"
	if len(addr) > 0 && addr[0] != ':' {
		url = "http://" + addr + "/health"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "Verifica /health local y termina con código 0 o 1")
	flag.Parse()

	cfg := config.Load()
	if *healthcheck {
		os.Exit(runHealthcheck(cfg.ListenAddr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := db.New(ctx, cfg.PostgresURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[api] Error conectando a PostgreSQL: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	pub, err := queue.NewPublisher(ctx, cfg.RabbitMQHost, cfg.RabbitMQUser, cfg.RabbitMQPass, cfg.DLXExchange)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[api] Error conectando a RabbitMQ: %v\n", err)
		os.Exit(1)
	}
	defer pub.Close()

	if cfg.HotfolderEnabled {
		go hotfolder.Run(ctx, cfg, store, pub)
	}

	srv := httpapi.NewServer(cfg, store, pub)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		fmt.Printf("[api] Servidor REST Go escuchando en %s (mutool + pdfimages + libvips, max_dim=%dpx, dlx=%s)\n",
			cfg.ListenAddr, cfg.Converter.MaxDim, cfg.DLXExchange)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[api] Error del servidor HTTP: %v\n", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("[api] Apagando ordenadamente...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
