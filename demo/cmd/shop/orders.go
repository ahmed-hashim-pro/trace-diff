package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type item struct {
	ID         int    `json:"id"`
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	PriceCents int    `json:"price_cents"`
}

type order struct {
	ID       int    `json:"id"`
	Customer string `json:"customer"`
	Status   string `json:"status"`
	Items    []item `json:"items"`
}

type ordersService struct {
	db   *pgxpool.Pool
	lazy bool
}

func newOrders(ctx context.Context, url string, lazy bool) (*ordersService, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &ordersService{db: db, lazy: lazy}, nil
}

func (o *ordersService) close() { o.db.Close() }

func (o *ordersService) handler() http.Handler {
	mux := http.NewServeMux()
	route(mux, "GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		ord, err := o.load(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ord)
	})
	return mux
}

func (o *ordersService) load(ctx context.Context, id int) (*order, error) {
	const q = "SELECT id, customer, status FROM orders WHERE id = $1"
	sctx, span := dbSpan(ctx, "SELECT", "orders", q)
	var ord order
	err := o.db.QueryRow(sctx, q, id).Scan(&ord.ID, &ord.Customer, &ord.Status)
	endSpan(span, ignoreNoRows(err))
	if err != nil {
		return nil, err
	}
	if o.lazy {
		ord.Items, err = o.itemsLazy(ctx, id)
	} else {
		ord.Items, err = o.items(ctx, id)
	}
	return &ord, err
}

// items is the code before the change: one query for all of an order's items.
func (o *ordersService) items(ctx context.Context, orderID int) ([]item, error) {
	const q = "SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = $1 ORDER BY id"
	sctx, span := dbSpan(ctx, "SELECT", "order_items", q)
	rows, err := o.db.Query(sctx, q, orderID)
	if err != nil {
		endSpan(span, err)
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	endSpan(span, err)
	return items, err
}

// itemsLazy is the change under review, written the way an ORM's lazy
// association loads: the ids first, then each item when it is touched.
func (o *ordersService) itemsLazy(ctx context.Context, orderID int) ([]item, error) {
	const qIDs = "SELECT id FROM order_items WHERE order_id = $1 ORDER BY id"
	sctx, span := dbSpan(ctx, "SELECT", "order_items", qIDs)
	rows, err := o.db.Query(sctx, qIDs, orderID)
	if err != nil {
		endSpan(span, err)
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	endSpan(span, err)
	if err != nil {
		return nil, err
	}
	items := make([]item, 0, len(ids))
	for _, id := range ids {
		it, err := o.item(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

func (o *ordersService) item(ctx context.Context, id int) (item, error) {
	const q = "SELECT id, sku, qty, price_cents FROM order_items WHERE id = $1"
	sctx, span := dbSpan(ctx, "SELECT", "order_items", q)
	var it item
	err := o.db.QueryRow(sctx, q, id).Scan(&it.ID, &it.SKU, &it.Qty, &it.PriceCents)
	endSpan(span, err)
	return it, err
}

func ignoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
