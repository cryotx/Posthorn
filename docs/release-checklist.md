# Release checklist (v1.0.0)

<!-- Modified for the cryotx/posthorn distribution. -->

Tag-day procedure for the v1.0.0 release. Follow in order. The release workflow at [`.github/workflows/release.yml`](../.github/workflows/release.yml) takes over once the tag is pushed; everything before that is preparation you do locally.

## 0. Operator validation (gating)

The end-to-end smoke gates the tag. **Don't proceed if it fails.**

### Docker smoke test

```bash
docker build -t posthorn:smoke ./core   # from the repository root
mkdir -p /tmp/posthorn-smoke && cd /tmp/posthorn-smoke
cat > posthorn.toml <<'EOF'
[[endpoints]]
path = "/api/contact"
to = ["you@yourdomain.com"]
from = "Posthorn Test <noreply@yourdomain.com>"
required = ["name", "email", "message"]
subject = "Smoke: {{.name}}"
body = "From {{.name}} <{{.email}}>: {{.message}}"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
EOF

docker run --rm -d -p 8080:8080 \
  -v $PWD/posthorn.toml:/etc/posthorn/config.toml:ro \
  -e POSTMARK_API_KEY=$POSTMARK_API_KEY \
  --name posthorn-smoke posthorn:smoke
sleep 1

curl -i -X POST http://localhost:8080/api/contact \
  -d "name=Smoke" -d "email=test@example.com" -d "message=hello"

docker logs posthorn-smoke   # confirm structured JSON logs
docker stop posthorn-smoke
```

**Pass criteria:** HTTP 200 with submission_id in the response body, email arrives in your inbox.

Full procedure (multi-step, real-mail smoke): [docs/manual-test.md](./manual-test.md).

## 1. Update CHANGELOG date if needed

[`CHANGELOG.md`](../CHANGELOG.md) currently has `## [1.0.0] — 2026-05-16` as a placeholder. Update if the actual tag day differs.

## 2. Verify CI on main is green

```bash
gh run list --workflow=ci.yml --branch=main --limit=1
```

Must be ✅ before tagging.

## 3. Tag and push `v1.0.0`

```bash
git tag -a v1.0.0 -m "Posthorn v1.0.0 — first public release

HTTP form ingress, Postmark transport, standalone Docker container.
Full release notes in CHANGELOG.md."

git push origin v1.0.0
```

This fires [`.github/workflows/release.yml`](../.github/workflows/release.yml). Watch:

```bash
gh run watch $(gh run list --workflow=release.yml --limit=1 --json databaseId -q '.[0].databaseId')
```

The workflow:
1. Builds `linux/amd64` + `linux/arm64` via buildx
2. Tags as `:1.0.0`, `:1.0`, `:1`, `:latest` (because no pre-release suffix)
3. Pushes to `ghcr.io/cryotx/posthorn`

**Pass criteria:** workflow green, `docker pull ghcr.io/cryotx/posthorn:1.0.0` works on both archs, `docker pull ghcr.io/cryotx/posthorn:latest` resolves to the same digest.

## 4. Create the GitHub Release

```bash
# Extracts everything from the v1.0.0 heading to EOF. v1.0.0 is the last
# entry in CHANGELOG.md so EOF is the right terminator; for a later patch
# release that's no longer true, swap the awk for a `[1.0.0]...[other]`
# range expression.
gh release create v1.0.0 \
  --title "v1.0.0 — first public release" \
  --notes "$(awk '/^## \[1\.0\.0\]/{flag=1} flag' CHANGELOG.md)"
```

Or use the GitHub UI to paste the same notes.
