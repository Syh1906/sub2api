-- 客户收到的响应编号可与计费去重编号不同，旧记录保留原编号。
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS client_request_id VARCHAR(64);
