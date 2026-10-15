package main

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
)

// newPricing quotes an order. Nine requests in ten hit its cache and take
// about 8 ms; the tenth fetches rates from a slow upstream, 45 to 80 ms.
func newPricing() http.Handler {
	mux := http.NewServeMux()
	route(mux, "GET /quote/{order}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if rand.Float64() < 0.10 {
			fetchRates(ctx, time.Duration(45+rand.IntN(36))*time.Millisecond)
		}
		sleep(ctx, time.Duration(8*math.Exp(0.3*rand.NormFloat64())*float64(time.Millisecond)))
		total, _ := strconv.Atoi(r.URL.Query().Get("total"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"order": r.PathValue("order"), "total_cents": total * 119 / 100, "currency": "EUR",
		})
	})
	return mux
}

func fetchRates(ctx context.Context, d time.Duration) {
	ctx, span := otel.Tracer("shop/pricing").Start(ctx, "fetch rates")
	defer span.End()
	sleep(ctx, d)
}

// sleep waits for d. Like most handlers, it does not notice that the caller
// has gone.
func sleep(_ context.Context, d time.Duration) { time.Sleep(d) }
