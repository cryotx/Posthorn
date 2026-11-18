# Run Posthorn with Docker Compose

This example builds the project from source, stores its SQLite database under `./data`, and listens on loopback. The initial configuration previews messages locally. It requires Docker Compose v2 or newer, Bash and OpenSSL.

## Start a fresh installation

From this directory, create a private environment file and generate a client key. The shell's noclobber option protects an existing file.

```bash
(
  set -C
  umask 077
  { cat .env.example; printf '\nPOSTHORN_API_KEY=%s\n' "$(openssl rand -hex 32)"; } > .env
)
```

On Linux with a standard Docker daemon, give the image's unprivileged user (UID and GID 65532) a writable data directory. If your daemon uses user namespace remapping, use the host UID and GID mapped to this container user. Set `POSTHORN_HTTP_PORT` in `.env` to an available port.

```bash
sudo install -d -m 0700 -o 65532 -g 65532 ./data
docker compose config --quiet
docker compose build
docker compose run --rm --no-deps --entrypoint /usr/local/bin/posthorn posthorn validate --config /etc/posthorn/config.toml
docker compose up -d
docker compose ps
```

## Preview a message

```bash
base_url="http://$(docker compose port posthorn 8080)"
client_key="$(sed -n 's/^POSTHORN_API_KEY=//p' .env)"
curl --fail-with-body "$base_url/readyz"
curl --fail-with-body "$base_url/api/send" \
  -H "Authorization: Bearer $client_key" \
  -H 'Content-Type: application/json' \
  --data '{"subject":"Gateway check","message":"Hello from Posthorn"}'
```

The response includes `"status":"dry_run"` and the rendered message. `POSTHORN_TO` supplies the default recipient. An authenticated caller can pass the JSON string field `to_override` for a single submission.

## Enable delivery

Set `RESEND_API_KEY` to a sending token, `POSTHORN_FROM` to a sender on a verified domain, and `POSTHORN_TO` to your intended default recipient. Set `POSTHORN_DRY_RUN=false` and run `docker compose up -d` to apply the environment changes. The same request now sends an email. If you expose the API to other servers, use a TLS reverse proxy and pass through only the routes they need.

## Administration and backups

To enable the admin dashboard, add a separate random token to the private `.env` file as `POSTHORN_ADMIN_TOKEN`. Generate it with `openssl rand -hex 32`. Run `docker compose up -d`, then open `/admin/login` at the local service address. The dashboard shows deliveries, providers and queued messages. Treat both the admin token and the client key as credentials.

Use the backup command to take a consistent database snapshot while Posthorn is running.

```bash
docker compose exec posthorn /usr/local/bin/posthorn backup --config /etc/posthorn/config.toml
```

The command prints the path to the snapshot in the mounted data directory. Keep the snapshot, the private environment file and the separate `provider-secrets.key` file (when present) in protected backup storage. The key is needed to decrypt credentials for providers saved through the admin dashboard.
