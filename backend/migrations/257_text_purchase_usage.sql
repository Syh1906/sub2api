-- 已有 spent 只保存合计，升级时不能从它推断具体请求。
ALTER TABLE text_purchase_vouchers
    ADD COLUMN historical_spent numeric(20,8) NOT NULL DEFAULT 0,
    ADD COLUMN adjusted_points numeric(20,8) NOT NULL DEFAULT 0;
UPDATE text_purchase_vouchers SET historical_spent = spent WHERE spent > 0;

CREATE TABLE text_purchase_usage (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id bigint NOT NULL REFERENCES text_purchase_vouchers(order_id),
    request_id text NOT NULL,
    api_key_id bigint NOT NULL,
    points numeric(20,8) NOT NULL CHECK (points > 0),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    UNIQUE (order_id, request_id, api_key_id)
);
CREATE INDEX text_purchase_usage_order_page ON text_purchase_usage(order_id, created_at DESC, id DESC);

-- 账务事务在余额 UPDATE 前通过 set_config(..., true) 传入已经认领的请求。
-- 直接调减余额仍影响可退积分，但没有模型请求编号，单独记为 adjusted_points。
CREATE OR REPLACE FUNCTION allocate_text_purchase_spend() RETURNS trigger LANGUAGE plpgsql AS $body$
DECLARE
    available numeric;
    shortfall numeric;
    request_start numeric;
    requests jsonb;
BEGIN
    SELECT sum(points-spent) INTO available FROM text_purchase_vouchers
     WHERE user_id=NEW.id AND redeemed_at IS NOT NULL AND NOT refund_hold AND spent < points;
    shortfall := available - greatest(NEW.balance + coalesce(NEW.frozen_balance,0), 0);
    IF shortfall IS NULL OR shortfall <= 0 THEN RETURN NEW; END IF;

    requests := coalesce(nullif(current_setting('sub2api.billing_requests', true), ''), '[]')::jsonb;
    -- 以第一笔未用购买积分为坐标 0。负起点代表先使用普通余额。
    request_start := available - (OLD.balance + coalesce(OLD.frozen_balance,0));
    WITH lots AS (
        SELECT order_id, points-spent AS amount,
               coalesce(sum(points-spent) OVER (ORDER BY redeemed_at, order_id
                      ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) AS start_at
          FROM text_purchase_vouchers
         WHERE user_id=NEW.id AND redeemed_at IS NOT NULL AND NOT refund_hold AND spent < points
    ), used_lots AS (
        SELECT order_id, start_at, least(start_at + amount, shortfall) AS end_at
          FROM lots WHERE start_at < shortfall
    ), request_costs AS (
        SELECT r.request_id, r.api_key_id, r.ord,
               CASE WHEN jsonb_array_length(requests) = 1
                    THEN (OLD.balance + coalesce(OLD.frozen_balance,0))
                         - (NEW.balance + coalesce(NEW.frozen_balance,0))
                    ELSE r.cost END AS cost
          FROM ROWS FROM (jsonb_to_recordset(requests) AS
              (request_id text, api_key_id bigint, cost numeric)) WITH ORDINALITY
              AS r(request_id, api_key_id, cost, ord)
    ), debits AS (
        SELECT r.*, request_start + sum(cost) OVER (ORDER BY ord) AS end_at FROM request_costs r
    ), recorded AS (
        INSERT INTO text_purchase_usage(order_id, request_id, api_key_id, points)
        SELECT l.order_id, d.request_id, d.api_key_id,
               least(l.end_at, d.end_at) - greatest(l.start_at, d.end_at-d.cost)
          FROM used_lots l JOIN debits d
            ON d.end_at > l.start_at AND d.end_at-d.cost < l.end_at
         ORDER BY d.ord, l.order_id
        RETURNING order_id, points
    ), attributed AS (
        SELECT order_id, sum(points) AS amount FROM recorded GROUP BY order_id
    )
    UPDATE text_purchase_vouchers v
       SET spent = v.spent + (l.end_at-l.start_at),
           adjusted_points = v.adjusted_points + (l.end_at-l.start_at) - coalesce(a.amount,0)
      FROM used_lots l LEFT JOIN attributed a USING (order_id)
     WHERE v.order_id=l.order_id;
    RETURN NEW;
END
$body$;
