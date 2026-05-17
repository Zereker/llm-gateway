-- MySQL 启动期初始化脚本：创建 Debezium 专用账号。
--
-- docker-entrypoint-initdb.d 目录下的 .sql 文件在容器首次启动时自动跑。
-- 已有 volume 数据时不会重跑（mysql 通过检查 mysql.user 表已存在判断）。

-- Debezium 抓 binlog 需要 REPLICATION SLAVE + REPLICATION CLIENT 权限。
-- SELECT / RELOAD 用于 snapshot 阶段（初次全量读）。
-- SHOW DATABASES 用于元数据查询。
CREATE USER IF NOT EXISTS 'debezium'@'%' IDENTIFIED BY 'debezium-pass';
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'debezium'@'%';
FLUSH PRIVILEGES;
