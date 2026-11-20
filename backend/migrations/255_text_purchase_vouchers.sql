-- 每笔已付款的商品只发一个码。余额仍由 users 和现有计费事务维护。
CREATE TABLE IF NOT EXISTS text_purchase_vouchers (
    order_id bigint PRIMARY KEY REFERENCES payment_orders(id),
    user_id bigint NOT NULL REFERENCES users(id),
    redeem_id bigint NOT NULL UNIQUE REFERENCES redeem_codes(id),
    transaction_id text NOT NULL UNIQUE,
    merchant_order_id text NOT NULL,
    paid_cents bigint NOT NULL CHECK (paid_cents > 0),
    redeemed_at timestamptz,
    points numeric(20,8) NOT NULL CHECK (points > 0),
    spent numeric(20,8) NOT NULL DEFAULT 0 CHECK (spent >= 0 AND spent <= points),
    refund_hold boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS text_purchase_vouchers_unspent
    ON text_purchase_vouchers(user_id, redeemed_at, order_id)
    WHERE redeemed_at IS NOT NULL AND NOT refund_hold AND spent < points;

-- 普通余额先用；需要动用购买积分时，按兑换时间、订单编号依次归属。
-- 冻结余额和可用余额互转时总额不变。账务已锁住用户行，
-- 同一客户的兑换、扣费和退款均按用户行、积分记录的顺序串行。
CREATE OR REPLACE FUNCTION allocate_text_purchase_spend() RETURNS trigger LANGUAGE plpgsql AS $body$
DECLARE
    shortfall numeric;
    portion numeric;
    lot record;
BEGIN
    SELECT sum(points-spent) - greatest(NEW.balance + coalesce(NEW.frozen_balance,0),0)
      INTO shortfall FROM text_purchase_vouchers
     WHERE user_id=NEW.id AND redeemed_at IS NOT NULL AND NOT refund_hold AND spent < points;
    IF shortfall IS NULL OR shortfall <= 0 THEN RETURN NEW; END IF;
    FOR lot IN SELECT order_id, points-spent AS remaining FROM text_purchase_vouchers
                WHERE user_id=NEW.id AND redeemed_at IS NOT NULL AND NOT refund_hold AND spent < points
                ORDER BY redeemed_at, order_id FOR UPDATE LOOP
        portion := least(shortfall, lot.remaining);
        UPDATE text_purchase_vouchers SET spent=spent+portion WHERE order_id=lot.order_id;
        shortfall := shortfall-portion;
        EXIT WHEN shortfall <= 0;
    END LOOP;
    RETURN NEW;
END
$body$;
CREATE TRIGGER allocate_text_purchase_spend AFTER UPDATE OF balance, frozen_balance ON users
FOR EACH ROW WHEN (NEW.balance + coalesce(NEW.frozen_balance,0) < OLD.balance + coalesce(OLD.frozen_balance,0))
EXECUTE FUNCTION allocate_text_purchase_spend();
