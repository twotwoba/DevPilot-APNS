#!/usr/bin/env bash
# relay 每日在线快照 + 7 天留存清理(03:00 UTC systemd timer 触发)。
# 快照走 /admin/snapshot(进程内 bbolt 备份,bbolt flock 使 CLI 子命令
# 在 relay 运行时不可用);清理用 alpine 容器删卷内第 8 个以前的快照。
set -euo pipefail

cd /home/ec2-user/DevPilot-APNS/deploy
set -a; . ./.env; set +a

curl -fsS -m 60 -X POST \
  -H "Authorization: Bearer ${METRICS_TOKEN:?METRICS_TOKEN missing in .env}" \
  https://relay.devpilot.app/admin/snapshot >> /home/ec2-user/snapshot-cron.log 2>&1

docker run --rm -v deploy_relay-data:/data alpine:3.20 \
  sh -c 'cd /data/snapshots && ls -1t | tail -n +8 | xargs -r rm --' \
  >> /home/ec2-user/snapshot-cron.log 2>&1
