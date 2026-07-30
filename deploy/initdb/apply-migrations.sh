#!/bin/sh
# postgres 官方镜像启动时会按文件名顺序执行 /docker-entrypoint-initdb.d 下的
# *.sh/*.sql；这里用一个脚本按文件名顺序（000001...000005...）只跑 /migrations 下的
# *.up.sql，跳过 *.down.sql（down 脚本只在需要回滚时手动执行，不属于建库流程）。
#
# 注意：此脚本只在命名卷首次创建时执行一次。后续新增的迁移文件不会被自动应用，
# 见 deploy/docker-compose.yml 顶部注释的两种手动应用方式。
set -e

for f in $(ls /migrations/*.up.sql | sort); do
    echo "applying migration: $f"
    psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f "$f"
done
