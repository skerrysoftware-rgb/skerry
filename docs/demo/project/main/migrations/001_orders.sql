CREATE TABLE orders (
  id         serial PRIMARY KEY,
  item       text NOT NULL,
  status     text NOT NULL DEFAULT 'new',
  created_at timestamptz NOT NULL DEFAULT now()
);
