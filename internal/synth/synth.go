// Package synth generates traces of a small shop, with or without a planted
// change, so trace-diff can be measured on known answers: does it find the
// change that was planted, and does it stay quiet when nothing changed?
//
// The shop has four endpoints on a gateway: GET /orders/{id} (users,
// orders and pricing behind it), GET /orders (a list that already calls the
// catalog once per order, on both sides, so per-item calls are not news by
// themselves), POST /checkout (with a payments call that is retried on 3%
// of requests on both sides), and GET /health. Latencies are log-normal,
// the pricing service has a cache that hits 70% of the time, queries inline
// literal ids the way some ORMs do, and the traffic mix moves by up to ±50%
// between the two sides, as it does between a baseline and a canary.
package synth

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

// Scenarios that can be planted in the after side.
var Scenarios = []struct{ Name, What string }{
	{"none", "no change: two runs of the same code"},
	{"n-plus-one", "orders loads each item with its own query"},
	{"new-sync-call", "orders checks stock with the inventory service before answering"},
	{"new-hop", "the users lookup moves from the gateway to the orders service"},
	{"retry-storm", "the gateway retries pricing with a timeout that 25% of calls exceed"},
	{"fan-out", "pricing asks 8 rate shards in parallel instead of 2"},
	{"latency", "the orders lookup loses its index: the query takes 4x as long"},
	{"drift", "small real shifts that should not be reported: the pricing cache hits 64% instead of 70%, user lookups take 8% longer, payments are retried on 4% of checkouts instead of 3%"},
}

// Traces is how many requests per side a scenario is measured with, given
// the usual n. Drift is measured with ten times as many, since changes that
// small only become statistically significant with a lot of traffic, and
// that is when the size floors have to keep them quiet.
func Traces(scenario string, n int) int {
	if scenario == "drift" {
		return 10 * n
	}
	return n
}

// Generate makes n traces for one side. The after side of a scenario has
// the change planted; seed and side pick the random stream, so before and
// after are independent runs.
func Generate(scenario string, after bool, seed int64, n int) ([]otlp.Span, error) {
	known := false
	for _, s := range Scenarios {
		known = known || s.Name == scenario
	}
	if !known {
		return nil, fmt.Errorf("unknown scenario %q", scenario)
	}
	sideSeed := seed*2 + 1
	if after {
		sideSeed++
	}
	g := &gen{
		rng: rand.New(rand.NewSource(sideSeed)),
		t:   1_760_000_000_000_000_000 + sideSeed*3_600_000_000_000,
	}
	if after {
		g.change = scenario
	}
	weights := []float64{0.45, 0.25, 0.20, 0.10}
	var total float64
	for i := range weights {
		weights[i] *= 1 + (g.rng.Float64() - 0.5)
		total += weights[i]
	}
	for i := 0; i < n; i++ {
		x := g.rng.Float64() * total
		switch {
		case x < weights[0]:
			g.getOrder()
		case x < weights[0]+weights[1]:
			g.listOrders()
		case x < weights[0]+weights[1]+weights[2]:
			g.checkout()
		default:
			g.health()
		}
		g.t += int64(5+g.rng.Intn(40)) * 1_000_000
	}
	return g.spans, nil
}

type gen struct {
	rng    *rand.Rand
	t      int64 // start of the next request, ns
	change string
	spans  []otlp.Span
	trace  string
}

const msec = int64(1_000_000)

func (g *gen) id(n int) string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 2*n)
	for i := range b {
		b[i] = hexd[g.rng.Intn(16)]
	}
	return string(b)
}

// lat is a log-normal latency with the given median, in ns.
func (g *gen) lat(medianMs float64) int64 {
	return int64(medianMs * math.Exp(0.35*g.rng.NormFloat64()) * float64(msec))
}

// node is a span being built; its children are laid out by the caller.
type node struct {
	g    *gen
	span *otlp.Span
}

func (g *gen) open(parent *node, service string, kind otlp.Kind, name string, start int64, attrs map[string]string) *node {
	s := otlp.Span{
		TraceID: g.trace, SpanID: g.id(8), Name: name, Kind: kind,
		Service: service, Start: start, End: start, Attrs: attrs,
	}
	if parent != nil {
		s.ParentID = parent.span.SpanID
	}
	g.spans = append(g.spans, s)
	return &node{g, &g.spans[len(g.spans)-1]}
}

// The spans slice may move when it grows, so nodes keep indexes, not
// pointers, in practice: fix up by span id when closing.
func (n *node) close(end int64, err bool) int64 {
	for i := len(n.g.spans) - 1; i >= 0; i-- {
		if n.g.spans[i].SpanID == n.span.SpanID {
			n.g.spans[i].End = end
			n.g.spans[i].Err = err
			n.span = &n.g.spans[i]
			break
		}
	}
	return end
}

func (n *node) ref() *node {
	for i := len(n.g.spans) - 1; i >= 0; i-- {
		if n.g.spans[i].SpanID == n.span.SpanID {
			n.span = &n.g.spans[i]
			break
		}
	}
	return n
}

// query adds a database span and returns its end.
func (g *gen) query(parent *node, service, system, stmt string, start int64, medianMs float64) int64 {
	n := g.open(parent.ref(), service, otlp.KindClient, "query", start, map[string]string{
		"db.system.name": system, "db.query.text": stmt,
	})
	return n.close(start+g.lat(medianMs), false)
}

// call adds an HTTP client span, the server span it reaches and whatever
// work handle does there; it returns the client span's end.
func (g *gen) call(parent *node, from, to, method, path, route string, start int64, fail bool, handle func(srv *node, at int64) int64) int64 {
	host := to
	c := g.open(parent.ref(), from, otlp.KindClient, method, start, map[string]string{
		"http.request.method": method, "url.full": "http://" + host + path, "server.address": host,
	})
	net := g.lat(0.4)
	srvStart := start + net
	if fail {
		// A timeout: the client gives up; the server's span is lost with it.
		return c.close(start+g.lat(30), true)
	}
	srv := g.open(c, to, otlp.KindServer, method+" "+route, srvStart, map[string]string{
		"http.request.method": method, "http.route": route, "url.path": path,
	})
	end := handle(srv, srvStart+g.lat(0.2))
	end = srv.close(end+g.lat(0.2), false)
	return c.close(end+g.lat(0.4), false)
}

func (g *gen) root(route, method, path string) *node {
	g.trace = g.id(16)
	return g.open(nil, "gateway", otlp.KindServer, method+" "+route, g.t, map[string]string{
		"http.request.method": method, "http.route": route, "url.path": path,
	})
}

func (g *gen) user(parent *node, from string, at int64) int64 {
	uid := 1 + g.rng.Intn(5000)
	fail := g.rng.Float64() < 0.005
	return g.call(parent, from, "users", "GET", fmt.Sprintf("/users/%d", uid), "/users/{id}", at, fail, func(srv *node, t int64) int64 {
		med := 1.2
		if g.change == "drift" {
			med *= 1.08
		}
		return g.query(srv, "users", "postgresql", fmt.Sprintf("SELECT id, name, tier FROM users WHERE id = %d", uid), t, med)
	})
}

func (g *gen) getOrder() {
	oid := 1 + g.rng.Intn(100000)
	items := 1 + int(g.rng.ExpFloat64()*3)
	if g.rng.Float64() < 0.05 {
		items = 50
	}
	items = min(items, 50)
	r := g.root("/orders/{id}", "GET", fmt.Sprintf("/orders/%d", oid))
	t := r.span.Start + g.lat(0.3)
	if g.change != "new-hop" {
		t = g.user(r, "gateway", t)
	}
	t = g.call(r, "gateway", "orders", "GET", fmt.Sprintf("/orders/%d", oid), "/orders/{id}", t, false, func(srv *node, at int64) int64 {
		med := 2.0
		if g.change == "latency" {
			med = 8.0
		}
		at = g.query(srv, "orders", "postgresql", "SELECT id, user_id, status FROM orders WHERE id = $1", at, med)
		if g.change == "n-plus-one" {
			at = g.query(srv, "orders", "postgresql", "SELECT id FROM order_items WHERE order_id = $1", at, 0.8)
			for i := 0; i < items; i++ {
				at = g.query(srv, "orders", "postgresql", "SELECT id, sku, qty, price_cents FROM order_items WHERE id = $1", at, 0.6)
			}
		} else {
			at = g.query(srv, "orders", "postgresql", "SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = $1", at, 1.5)
		}
		if g.change == "new-hop" {
			at = g.user(srv, "orders", at)
		}
		if g.change == "new-sync-call" {
			sku := fmt.Sprintf("SKU-%05d", g.rng.Intn(20000))
			at = g.call(srv, "orders", "inventory", "GET", "/stock/"+sku, "/stock/{sku}", at, false, func(inv *node, t int64) int64 {
				return g.query(inv, "inventory", "postgresql", fmt.Sprintf("SELECT available FROM stock WHERE sku = '%s'", sku), t, 6)
			})
		}
		return at
	})
	t = g.quote(r, oid, t)
	r.close(t+g.lat(0.3), false)
}

// quote asks pricing for a quote: a cache, a database on a miss, and rate
// shards in parallel.
func (g *gen) quote(r *node, oid int, t int64) int64 {
	attempts := 1
	if g.change == "retry-storm" {
		attempts = 3
	}
	path := fmt.Sprintf("/quote/%d", oid)
	for a := 0; a < attempts; a++ {
		fail := g.change == "retry-storm" && a < attempts-1 && g.rng.Float64() < 0.25
		t = g.call(r, "gateway", "pricing", "GET", path, "/quote/{order}", t, fail, func(srv *node, at int64) int64 {
			end := g.query(srv, "pricing", "redis", fmt.Sprintf("GET quote:%d", oid), at, 0.3)
			hit := 0.7
			if g.change == "drift" {
				hit = 0.64
			}
			if g.rng.Float64() >= hit {
				n := 1 + g.rng.Intn(6)
				in := "1"
				for i := 1; i < n; i++ {
					in += fmt.Sprintf(", %d", 1+g.rng.Intn(9999))
				}
				end = g.query(srv, "pricing", "postgresql", "SELECT sku, price_cents FROM prices WHERE sku IN ("+in+")", end, 2.5)
			}
			shards := 2
			if g.change == "fan-out" {
				shards = 8
			}
			last := end
			for s := 0; s < shards; s++ {
				e := g.call(srv, "pricing", "rates", "GET", fmt.Sprintf("/shards/%d/rates", s+1), "/shards/{shard}/rates", end+g.lat(0.05), false, func(rs *node, at int64) int64 {
					return at + g.lat(1.5)
				})
				last = max(last, e)
			}
			return last
		})
		if !fail {
			break
		}
	}
	return t
}

func (g *gen) listOrders() {
	uid := 1 + g.rng.Intn(5000)
	r := g.root("/orders", "GET", fmt.Sprintf("/orders?user=%d", uid))
	n := 1 + int(g.rng.ExpFloat64()*4)
	n = min(n, 20)
	t := g.user(r, "gateway", r.span.Start+g.lat(0.3))
	t = g.call(r, "gateway", "orders", "GET", fmt.Sprintf("/orders?user=%d", uid), "/orders", t, false, func(srv *node, at int64) int64 {
		return g.query(srv, "orders", "postgresql", "SELECT id, status FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT 20", at, 3)
	})
	for i := 0; i < n; i++ {
		pid := 1 + g.rng.Intn(20000)
		t = g.call(r, "gateway", "catalog", "GET", fmt.Sprintf("/products/%d", pid), "/products/{id}", t, false, func(srv *node, at int64) int64 {
			return g.query(srv, "catalog", "postgresql", fmt.Sprintf("SELECT id, title, image FROM products WHERE id = %d", pid), at, 0.9)
		})
	}
	r.close(t+g.lat(0.3), false)
}

func (g *gen) checkout() {
	r := g.root("/checkout", "POST", "/checkout")
	t := g.user(r, "gateway", r.span.Start+g.lat(0.3))
	t = g.call(r, "gateway", "orders", "POST", "/orders", "/orders", t, false, func(srv *node, at int64) int64 {
		at = g.query(srv, "orders", "postgresql", "INSERT INTO orders (user_id, status) VALUES ($1, $2) RETURNING id", at, 2)
		rows := "(1, 'SKU-00001', 1)"
		for i := 1 + g.rng.Intn(4); i > 0; i-- {
			rows += fmt.Sprintf(", (%d, 'SKU-%05d', %d)", 1+g.rng.Intn(1e6), g.rng.Intn(20000), 1+g.rng.Intn(3))
		}
		return g.query(srv, "orders", "postgresql", "INSERT INTO order_items (order_id, sku, qty) VALUES "+rows, at, 2.5)
	})
	retryRate := 0.03
	if g.change == "drift" {
		retryRate = 0.04
	}
	retry := g.rng.Float64() < retryRate
	t = g.call(r, "gateway", "payments", "POST", "/charge", "/charge", t, retry, func(srv *node, at int64) int64 {
		return at + g.lat(25)
	})
	if retry {
		t = g.call(r, "gateway", "payments", "POST", "/charge", "/charge", t, false, func(srv *node, at int64) int64 {
			return at + g.lat(25)
		})
	}
	r.close(t+g.lat(0.3), false)
}

func (g *gen) health() {
	r := g.root("/health", "GET", "/health")
	r.close(r.span.Start+g.lat(0.1), false)
}
