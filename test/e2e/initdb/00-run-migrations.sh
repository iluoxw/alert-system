#!/bin/sh
# postgres 官方镜像启动时会按文件名顺序执行 /docker-entrypoint-initdb.d 下的
# *.sh/*.sql；这里用一个脚本按文件名顺序执行 /migrations 下的
# *.up.sql，跳过 *.down.sql（down 脚本只在需要回滚时手动执行，不属于建库流程）。
set -e

for f in $(ls /migrations/*.up.sql | sort); do
    echo "applying migration: $f"
    psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f "$f"
done

# postgres 官方镜像会在执行 initdb 脚本前短暂启动一个临时服务；仅使用
# pg_isready 做 healthcheck 会让 compose 在迁移尚未结束、正式服务尚未重启时
# 误判为 ready。这个容器内临时文件只在全部迁移完成后创建，避免把测试控制信息写进
# 应用数据库 schema；正式 PostgreSQL 进程重启后该文件仍保留在容器文件系统中。
touch /tmp/e2e-init-ready
