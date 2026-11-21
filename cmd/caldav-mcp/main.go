package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/Seeridia/caldav-mcp-go/internal/caldav"
	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/Seeridia/caldav-mcp-go/internal/mcpserver"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("MCP_TOKEN")
	username := os.Getenv("CALDAV_USERNAME")
	password := os.Getenv("CALDAV_PASSWORD")
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, " \t\r\n") ||
		strings.TrimSpace(username) == "" || password == "" {
		return errors.New("set CALDAV_USERNAME, CALDAV_PASSWORD and a non-empty MCP_TOKEN without whitespace")
	}
	zoneName := env("TZ", "UTC")
	if zoneName == "Local" {
		return errors.New("TZ must be an explicit IANA time zone")
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		return errors.New("TZ must be an IANA time zone")
	}
	client, err := caldav.New(os.Getenv("CALDAV_URL"), username, password, zone)
	if err != nil {
		return err
	}

	router := http.NewServeMux()
	router.Handle("/mcp", mcpserver.New(calendar.NewService(client, zone), token))
	router.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{
		Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: router,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	finished := make(chan error, 1)
	go func() {
		slog.Info("MCP server listening", "address", server.Addr)
		finished <- server.ListenAndServe()
	}()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		err := <-finished
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
