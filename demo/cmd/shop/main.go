// Command shop is the demo for trace-diff: a three-service order page.
//
//	shop frontend   GET /orders/{id}: asks orders for the order, then pricing for a quote
//	shop orders     GET /orders/{id}: the order and its items, from Postgres
//	shop pricing    GET /quote/{order}: a quote, usually fast, sometimes slow
//	shop load       sends requests to the frontend and prints what a dashboard would show
//
// CHANGE=after switches on the pull request under review: orders loads
// items lazily, one query per item, and the frontend wraps the pricing call
// in a 40 ms timeout with up to three attempts. Both pass the tests in
// shop_test.go, and both look fine on a small order.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shop frontend|orders|pricing|load [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "frontend":
		err = serve(ctx, "frontend", newFrontend(env("ORDERS_URL", "http://orders:8081"), env("PRICING_URL", "http://pricing:8082"), after()))
	case "orders":
		var o *ordersService
		o, err = newOrders(ctx, env("DATABASE_URL", "postgres://postgres:shop@postgres:5432/postgres"), after())
		if err == nil {
			defer o.close()
			err = serve(ctx, "orders", o.handler())
		}
	case "pricing":
		err = serve(ctx, "pricing", newPricing())
	case "load":
		err = load(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown role %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shop:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func after() bool { return os.Getenv("CHANGE") == "after" }

// serve runs one service with tracing until ctx ends, then flushes spans.
func serve(ctx context.Context, service string, h http.Handler) error {
	shutdown, err := setupTracing(ctx, service)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/", h)
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "shop %s listening on %s (CHANGE=%s)\n", service, srv.Addr, env("CHANGE", "before"))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdown(sctx)
}
