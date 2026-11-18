# Posthorn

简体中文 · [English](README.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md)

Posthorn 是面向自托管应用的邮件发送网关。应用通过网页表单、JSON API 或 SMTP 提交邮件，Posthorn 再通过 Resend、Postmark、Mailgun、Amazon SES 等服务商发送。可配置多个服务商，按规则、配额和费用选择发送路径，或在临时故障时切换。

- 使用项目 API 密钥隔离不同应用，可在管理后台创建项目和更换密钥。
- 使用 SQLite 记录投递状态、重试队列和退信地址。通过管理后台保存的服务商凭据会加密存储。
- 登录页和管理后台支持简体中文、英语、日语、韩语和西班牙语。切换后会记住你的选择。

## 用 Docker Compose 预览邮件

需要 Docker Compose v2、Bash 和 OpenSSL。以下命令适用于使用标准 Docker 守护进程的 Linux 新环境。从本仓库源码构建镜像，默认在本机预览邮件。

```bash
git clone https://github.com/cryotx/Posthorn.git
cd Posthorn/deploy/compose
(
  set -C
  umask 077
  { cat .env.example; printf '\nPOSTHORN_API_KEY=%s\n' "$(openssl rand -hex 32)"; } > .env
)
sudo install -d -m 0700 -o 65532 -g 65532 ./data
docker compose up --build -d
```

容器就绪后，发送一条预览请求。默认配置会返回 `"status":"dry_run"` 和渲染后的邮件。

```bash
client_key="$(sed -n 's/^POSTHORN_API_KEY=//p' .env)"
curl -fsS http://127.0.0.1:8080/api/send \
  -H "Authorization: Bearer $client_key" \
  -H 'Content-Type: application/json' \
  --data '{"subject":"Posthorn 测试","message":"你好！"}'
```

正式发送前，请在私有 `.env` 中填入自己的服务商令牌、已验证的发件地址和默认收件地址，然后将 `POSTHORN_DRY_RUN` 设为 `false`。若要启用管理后台，在 `.env` 中另设 `POSTHORN_ADMIN_TOKEN`，重新运行 `docker compose up -d` 后访问 `http://127.0.0.1:8080/admin/login`。端口、配置验证和备份步骤见 [Compose 部署指南](deploy/compose/README.md)。

## 用 Go 构建

在仓库根目录运行以下命令，需要 Go 1.25 或更新版本。

```bash
make build
./bin/posthorn version
```

[配置示例](examples/)涵盖多服务商、项目隔离、配额和成本路由。[调用指南](docs/calling-posthorn.md)提供 Python 和 JavaScript 的 API 示例。

## 来源和许可证

本仓库基于 [craigmccaskill/posthorn](https://github.com/craigmccaskill/posthorn)，沿用 [Apache-2.0 许可证](LICENSE)。[posthorn.dev](https://posthorn.dev) 是上游文档站。
