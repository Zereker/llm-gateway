-- Runs once on first MySQL container init (empty data volume).
-- The main `llm_gateway` database is created via docker-compose's
-- MYSQL_DATABASE env; here we add a separate `llm_gateway_test` database so the
-- Go test suite (which TRUNCATEs tables between cases) never touches the
-- database a developer uses for manual/e2e data.
CREATE DATABASE IF NOT EXISTS llm_gateway_test
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
