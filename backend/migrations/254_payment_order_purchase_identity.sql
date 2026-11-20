ALTER TABLE payment_orders
    ADD COLUMN IF NOT EXISTS purchase_key_hash varchar(64),
    ADD COLUMN IF NOT EXISTS purchase_request_hash varchar(64),
    ADD COLUMN IF NOT EXISTS checkout_ciphertext text;

CREATE UNIQUE INDEX IF NOT EXISTS payment_orders_user_purchase_key
    ON payment_orders (user_id, purchase_key_hash);
