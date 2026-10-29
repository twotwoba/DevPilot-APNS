# relay deployment runbook (EC2 + Docker Compose)

Target topology: one Ubuntu 24.04 EC2 instance; Caddy terminates TLS and
reverse-proxies WSS to the relay; the relay and its bbolt data volume run in
Docker. A single instance is a deliberate choice — WSS is a stateful
connection, and scaling out requires a shared routing layer first.

## 1. Prerequisites

1. An EC2 instance (t4g.small to start), an allocated Elastic IP, and a domain
   A record pointing at that IP.
2. Security group allowing only `22/tcp` (restricted to your IP), `80/tcp`,
   `443/tcp`, `443/udp`.
3. Install Docker and the compose plugin, and verify clock sync:

   ```bash
   sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 chrony
   sudo systemctl enable --now chrony
   timedatectl status   # System clock synchronized: yes
   ```

   The JWT `iat` is clock-sensitive; too much skew gets a 403 from APNs.

## 2. Configuration

```bash
git clone https://github.com/twotwoba/DevPilot-APNS.git
cd DevPilot-APNS/deploy
cp .env.example .env && chmod 600 .env
mkdir -p secrets && chmod 700 secrets
# Copy the downloaded AuthKey_XXXX.p8 to secrets/apns.p8
cp ~/Downloads/AuthKey_XXXX.p8 secrets/apns.p8 && chmod 600 secrets/apns.p8
$EDITOR .env   # fill in DOMAIN / APNS_KEY_ID / APNS_TEAM_ID / METRICS_TOKEN
               # TRUSTED_PROXY_COUNT=1 (required behind the Caddy reverse proxy,
               # so the real client IP is recovered from XFF)
```

On Linux, bind mounts preserve the host file's ownership, while the image runs
as distroless `nonroot` (uid/gid 65532). The `.p8` must be handed to that uid,
otherwise the key is unreadable inside the container and `/readyz` returns 503:

```bash
sudo chown 65532:65532 secrets/apns.p8
chmod 600 secrets/apns.p8
```

(The macOS + OrbStack mount layer remaps ownership, so this is usually
unnecessary there; on EC2 it is required.)

Both `.env` and `.p8` are excluded by `.gitignore` and `.dockerignore` and
never enter the image.

## 3. Start & self-check

```bash
docker compose pull && docker compose up -d
docker compose ps
curl -fsS https://relay.example.com/healthz   # ok
curl -fsS https://relay.example.com/readyz    # ready (includes APNs key availability)
curl -fsS https://relay.example.com/metrics | head
```

If `/readyz` returns 503, check `docker compose logs relay`: usual causes are
the `.p8` path, a Key ID / Team ID mismatch, or a wrong environment
(`production` / `sandbox`).

## 4. Pairing & integration testing

On the dev machine:

```bash
devpilot-hook install
devpilot-hook pair --relay https://relay.example.com
```

Enter the pairing code in the app's "Add Dev Machine". The app and the daemon
then connect to `/ws/app` and `/ws/daemon` respectively; you can watch
`devpilot_daemon_connections` and `devpilot_app_connections` on `/metrics`.

## 5. Upgrades

```bash
cd DevPilot-APNS/deploy
docker compose pull && docker compose up -d
```

The bbolt schema is managed via `meta.schema_version`; there are no breaking
migrations at the moment. To roll back, point `image:` back at the previous tag.

## 6. Backups

The relay ships a consistent snapshot subcommand (bbolt `View → tx.WriteTo`):

```bash
docker compose exec relay /usr/local/bin/devpilot-relay snapshot --out /data/snapshots/relay-$(date +%F).db
```

Run it daily from a host cron job and sync `/data/snapshots` to offsite
storage. A snapshot can restore by replacing `/data/relay.db` while the relay
is stopped.

## 7. Monitoring & alerting

At minimum, watch:

- `/readyz` returning non-200;
- `devpilot_apns_pushes_total{status="410"}` (token invalidated; clients re-register);
- `devpilot_apns_pushes_total{status="429"}` (rate limited);
- `devpilot_apns_pushes_total{status="403"}` (key/clock problem — must alert);
- `devpilot_pending_approvals` staying high (user phones not receiving notifications).

Logs are structured JSON (docker json-file driver, rotation capped).

## 8. `.p8` and app-side checklist (run once credentials are available)

1. Apple Developer → Certificates, Identifiers & Profiles → Keys → `+` →
   Apple Push Notifications service (APNs); download the `.p8`
   (**downloadable only once**).
2. Note the Key ID; the Team ID is on the Membership page (`D8N2Q2WZ86` for
   this project).
3. Enable Push Notifications and Time Sensitive Notifications on App ID
   `com.twotwoba.devpilot`, then regenerate the provisioning profile.
4. Add the `aps-environment` and
   `com.apple.developer.usernotifications.time-sensitive` entitlements to the
   iOS project.
5. Validate first with `APNS_ENVIRONMENT=sandbox` + a development device
   token, then switch to `production`.
6. The Live Activity topic must be
   `com.twotwoba.devpilot.push-type.liveactivity`.

## 9. Troubleshooting

| Symptom | Diagnosis |
|---|---|
| WebSocket drops immediately after upgrade | Caddy missing `flush_interval -1`; or security group not allowing 443 |
| `/readyz` 503 | `.p8` unreadable / wrong Key ID / wrong Team ID |
| APNs 403 `ExpiredProviderToken` | Server clock drift; check chrony |
| APNs 403 `BadCertificateEnvironment` | sandbox token sent to production |
| Pairing code invalid | Expired after 15 minutes or already used; run `pair` again |
| 417/429 | Same topic sent too fast; the relay backs off built-in — if persistent, scale up or throttle |
