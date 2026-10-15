package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type frontend struct {
	orders, pricing string
	client          *http.Client
	retry           bool
}

func newFrontend(ordersURL, pricingURL string, retry bool) http.Handler {
	f := &frontend{orders: ordersURL, pricing: pricingURL, client: tracedClient(), retry: retry}
	mux := http.NewServeMux()
	route(mux, "GET /orders/{id}", f.order)
	return mux
}

type page struct {
	Order *order         `json:"order"`
	Quote map[string]any `json:"quote"`
}

func (f *frontend) order(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var p page
	status, err := f.get(ctx, f.orders+"/orders/"+r.PathValue("id"), &p.Order)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	total := 0
	for _, it := range p.Order.Items {
		total += it.Qty * it.PriceCents
	}
	quoteURL := fmt.Sprintf("%s/quote/%d?total=%d", f.pricing, p.Order.ID, total)
	if f.retry {
		_, err = f.getWithRetry(ctx, quoteURL, &p.Quote)
	} else {
		_, err = f.get(ctx, quoteURL, &p.Quote)
	}
	if err != nil {
		p.Quote = nil // the page still renders, without a quote
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

// getWithRetry is the change under review: "pricing is sometimes slow, so
// time out after 40 ms and try again, up to three times".
func (f *frontend) getWithRetry(ctx context.Context, url string, into any) (int, error) {
	var status int
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		actx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		status, err = f.get(actx, url, into)
		cancel()
		if err == nil {
			return status, nil
		}
	}
	return status, err
}

func (f *frontend) get(ctx context.Context, url string, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s: %s %s", url, resp.Status, b)
	}
	return http.StatusOK, json.NewDecoder(resp.Body).Decode(into)
}
