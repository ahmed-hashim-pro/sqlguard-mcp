-- A small shop, seeded so `sqlguard serve` has something to guard the moment
-- the repo is cloned. Nothing here is real.
CREATE TABLE customers (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL,
    email   TEXT NOT NULL UNIQUE,
    country TEXT NOT NULL,
    joined  TEXT NOT NULL
);

CREATE TABLE orders (
    id          INTEGER PRIMARY KEY,
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    status      TEXT NOT NULL,
    total       REAL NOT NULL,
    placed_at   TEXT NOT NULL
);

CREATE TABLE order_items (
    id       INTEGER PRIMARY KEY,
    order_id INTEGER NOT NULL REFERENCES orders(id),
    sku      TEXT NOT NULL,
    quantity INTEGER NOT NULL,
    price    REAL NOT NULL
);

INSERT INTO customers (id, name, email, country, joined) VALUES
    (1, 'Amina Yusuf',    'amina@example.com',  'KE', '2025-03-14'),
    (2, 'Bruno Silva',    'bruno@example.com',  'BR', '2025-06-02'),
    (3, 'Chen Wei',       'chen@example.com',   'SG', '2025-09-21'),
    (4, 'Dana Kowalski',  'dana@example.com',   'PL', '2026-01-08'),
    (5, 'Emeka Obi',      'emeka@example.com',  'NG', '2026-04-30');

INSERT INTO orders (id, customer_id, status, total, placed_at) VALUES
    (1, 1, 'shipped',   129.98, '2026-07-02'),
    (2, 1, 'paid',       49.99, '2026-08-11'),
    (3, 2, 'pending',    215.50, '2026-08-19'),
    (4, 3, 'shipped',     18.00, '2026-08-22'),
    (5, 3, 'refunded',    76.25, '2026-08-28'),
    (6, 4, 'paid',       310.00, '2026-09-01'),
    (7, 5, 'pending',     22.40, '2026-09-02'),
    (8, 2, 'paid',        95.10, '2026-09-03');

INSERT INTO order_items (id, order_id, sku, quantity, price) VALUES
    (1, 1, 'KB-101', 1,  79.99), (2, 1, 'MS-220', 1,  49.99),
    (3, 2, 'MS-220', 1,  49.99), (4, 3, 'MON-27', 1, 199.00),
    (5, 3, 'CBL-3M', 2,   8.25), (6, 4, 'CBL-3M', 2,   9.00),
    (7, 5, 'KB-101', 1,  76.25), (8, 6, 'MON-27', 1, 199.00),
    (9, 6, 'DSK-01', 1, 111.00), (10, 7, 'CBL-3M', 2, 11.20),
    (11, 8, 'KB-101', 1, 95.10);
