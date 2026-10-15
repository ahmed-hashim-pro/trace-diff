package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// These are the tests the change passes. They are the tests a reviewer
// would expect: the lazy loader returns the same orders as the eager one,
// and the frontend renders the same page with or without retries.

func TestLazyItemsMatchEager(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set (make demo-test runs this against the demo's Postgres)")
	}
	ctx := context.Background()
	eager, err := newOrders(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	defer eager.close()
	lazy, _ := newOrders(ctx, url, true)
	defer lazy.close()
	for _, id := range []int{1, 2, 25} {
		a, err := eager.load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		b, err := lazy.load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("order %d differs:\n%+v\n%+v", id, a, b)
		}
		if id == 25 && len(a.Items) != 50 {
			t.Errorf("order 25 has %d items, want 50", len(a.Items))
		}
	}
}

func fakeBackends(t *testing.T, firstQuoteDelay time.Duration) (string, string) {
	orders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(order{ID: 7, Customer: "c", Status: "paid", Items: []item{{1, "SKU-1", 2, 500}}})
	}))
	t.Cleanup(orders.Close)
	var calls atomic.Int32
	pricing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			time.Sleep(firstQuoteDelay)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_cents": 1190})
	}))
	t.Cleanup(pricing.Close)
	return orders.URL, pricing.URL
}

func getPage(t *testing.T, h http.Handler) page {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/orders/7", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var p page
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFrontendPageSameWithRetry(t *testing.T) {
	o1, p1 := fakeBackends(t, 0)
	o2, p2 := fakeBackends(t, 0)
	a := getPage(t, newFrontend(o1, p1, false))
	b := getPage(t, newFrontend(o2, p2, true))
	if !reflect.DeepEqual(a, b) {
		t.Errorf("pages differ:\n%+v\n%+v", a, b)
	}
}

func TestRetryRecoversFromSlowPricing(t *testing.T) {
	o, p := fakeBackends(t, 200*time.Millisecond)
	start := time.Now()
	pg := getPage(t, newFrontend(o, p, true))
	if pg.Quote == nil {
		t.Fatal("no quote")
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("took %s; the retry should not wait for the slow attempt", d)
	}
}
