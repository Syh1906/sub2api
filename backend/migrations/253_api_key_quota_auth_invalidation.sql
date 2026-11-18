-- 仅调整上限且状态未变时，复用既有缓存失效队列。用量累加不会触发此任务。
CREATE FUNCTION enqueue_api_key_quota_invalidation() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM enqueue_auth_cache_invalidation(NEW.key);
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_api_key_quota_invalidation
AFTER UPDATE OF quota ON api_keys
FOR EACH ROW WHEN (OLD.quota IS DISTINCT FROM NEW.quota AND OLD.status IS NOT DISTINCT FROM NEW.status AND NEW.deleted_at IS NULL)
EXECUTE FUNCTION enqueue_api_key_quota_invalidation();
