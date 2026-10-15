-- 200 orders. Most have 1 to 9 items; every 25th has 50, the way a few
-- customers order in bulk.
CREATE TABLE orders (
    id       integer PRIMARY KEY,
    customer text    NOT NULL,
    status   text    NOT NULL
);

CREATE TABLE order_items (
    id          serial  PRIMARY KEY,
    order_id    integer NOT NULL REFERENCES orders (id),
    sku         text    NOT NULL,
    qty         integer NOT NULL,
    price_cents integer NOT NULL
);

CREATE INDEX order_items_order_id ON order_items (order_id);

INSERT INTO orders
SELECT g, 'customer-' || (g % 37), 'paid'
FROM generate_series(1, 200) AS g;

INSERT INTO order_items (order_id, sku, qty, price_cents)
SELECT o, 'SKU-' || lpad(((o * 31 + k) % 5000)::text, 5, '0'), 1 + k % 3, 500 + (o * k) % 4500
FROM generate_series(1, 200) AS o,
     LATERAL generate_series(1, CASE WHEN o % 25 = 0 THEN 50 ELSE 1 + (o * 7919) % 9 END) AS k
ORDER BY o, k;

ANALYZE;
