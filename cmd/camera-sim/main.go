// Command camera-sim is the demo's camera: it replays a prepared burst set
// over FTP into SFTPGo, frame by frame, like a camera uploading a burst
// (HACKATHON-PLAN.md §2, §4).
//
//	POST /shoot?set=<name>[&dup=1]  upload demo/sets/<name>/*.jpg in filename order;
//	                                dup=1 re-sends the first frame under a new name
//	GET  /sets                      list sets and their frame counts
//	GET  /healthz                   204
//
// Environment: HTTP_ADDR (:8080), SETS_DIR (/demo/sets), FTP_ADDR (sftpgo:21),
// FTP_USER (camera), FTP_PASSWORD (camera), FRAME_INTERVAL (300ms),
// UPLOAD_ATTEMPTS (3, per frame).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/geilername/keeper/internal/telemetry"
)

const service = "camera-sim"

func main() {
	// Distroless has no curl, so the compose healthcheck runs the binary.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, service)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	interval, err := time.ParseDuration(envOr("FRAME_INTERVAL", "300ms"))
	if err != nil || interval < 0 {
		slog.Error("invalid FRAME_INTERVAL", "value", os.Getenv("FRAME_INTERVAL"), "err", err)
		os.Exit(1)
	}
	attempts, err := strconv.Atoi(envOr("UPLOAD_ATTEMPTS", "3"))
	if err != nil || attempts < 1 {
		slog.Error("invalid UPLOAD_ATTEMPTS", "value", os.Getenv("UPLOAD_ATTEMPTS"))
		os.Exit(1)
	}
	s := &shooter{
		setsDir: envOr("SETS_DIR", "/demo/sets"),
		target: ftpTarget{
			Addr:     envOr("FTP_ADDR", "sftpgo:21"),
			User:     envOr("FTP_USER", "camera"),
			Password: envOr("FTP_PASSWORD", "camera"),
		},
		interval: interval,
		attempts: attempts,
		tracer:   otel.Tracer(service),
	}

	srv := &http.Server{
		Addr: addr(),
		Handler: otelhttp.NewHandler(newMux(s), service,
			otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if r.Pattern != "" {
					return r.Pattern
				}
				return r.Method
			})),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		// Lets a running burst finish (a few seconds) before exiting.
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	slog.InfoContext(ctx, "camera-sim listening", "addr", srv.Addr, "sets_dir", s.setsDir,
		"ftp_addr", s.target.Addr, "frame_interval", interval)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "err", err)
	}
}

func healthcheck() int {
	_, port, err := net.SplitHostPort(addr())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}

func addr() string { return envOr("HTTP_ADDR", ":8080") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
