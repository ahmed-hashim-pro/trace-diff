package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"
)

// load sends requests for random orders and prints what a latency
// dashboard would show for them.
func load(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	url := fs.String("url", "http://frontend:8080", "frontend base URL")
	n := fs.Int("requests", 300, "requests to send")
	conc := fs.Int("concurrency", 4, "requests in flight")
	seed := fs.Uint64("seed", 1, "picks the orders")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the frontend to come up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deadline := time.Now().Add(*wait)
	for {
		resp, err := http.Get(*url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("frontend not up after %s: %v", *wait, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	rng := rand.New(rand.NewPCG(*seed, 7))
	ids := make([]int, *n)
	for i := range ids {
		ids[i] = 1 + rng.IntN(200)
	}
	var mu sync.Mutex
	var lat []float64
	var failed, noQuote int
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				start := time.Now()
				resp, err := http.Get(fmt.Sprintf("%s/orders/%d", *url, id))
				var p page
				if err == nil {
					if resp.StatusCode == http.StatusOK {
						err = json.NewDecoder(resp.Body).Decode(&p)
					} else {
						err = fmt.Errorf("%s", resp.Status)
					}
					resp.Body.Close()
				}
				d := float64(time.Since(start).Microseconds()) / 1000
				mu.Lock()
				lat = append(lat, d)
				if err != nil {
					failed++
				} else if p.Quote == nil {
					noQuote++
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	sort.Float64s(lat)
	q := func(p float64) float64 { return lat[int(p*float64(len(lat)-1))] }
	fmt.Printf("%d requests, %d errors, %d without a quote; latency p50 %.1f ms, p95 %.1f ms, p99 %.1f ms\n",
		len(lat), failed, noQuote, q(0.5), q(0.95), q(0.99))
	return nil
}
