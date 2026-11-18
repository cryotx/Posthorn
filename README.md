<div align="center">
  <img src="site/src/assets/logo.svg" width="84" height="84" alt="">
  <h1>Posthorn</h1>
  <p><strong>面向自托管应用的邮件发送网关</strong></p>
  <p>通过 HTTP 表单、JSON API 或 SMTP 接入应用，统一管理邮件路由、服务商和投递记录。</p>
  <p><strong>简体中文</strong> · <a href="README.en.md">English</a> · <a href="README.ja.md">日本語</a> · <a href="README.ko.md">한국어</a> · <a href="README.es.md">Español</a></p>
  <p><a href="#快速开始">快速开始</a> · <a href="#接入与管理">接入与管理</a> · <a href="#文档与示例">文档与示例</a></p>
</div>

## 功能概览

| 功能 | 你可以做什么 |
| --- | --- |
| **多种接入方式** | 用网页表单接收联系请求，用 JSON API 发送通知和验证码，或通过 SMTP 接入 Ghost、Gitea 等应用。 |
| **多服务商路由** | 连接 Resend、Postmark、Mailgun、Amazon SES、Brevo 等服务商，也可使用 SMTP 中继。按规则、配额和费用选路，在临时故障时切换备用服务商。 |
| **项目级密钥** | 给每个应用分配独立的 API 密钥，在后台创建项目和轮换密钥。 |
| **投递与重试** | 用 SQLite 保存投递历史、重试队列和抑制列表，查看邮件经过了哪个服务商以及失败原因。 |
| **五语管理后台** | 在登录页或后台顶部切换简体中文、英语、日语、韩语和西班牙语。通过后台保存的服务商凭据会加密存储。 |

## 快速开始

以下步骤适用于使用标准 Docker 守护进程的 Linux 环境，需要 Docker Compose v2、Bash 和 OpenSSL。示例从源码构建镜像，默认只在本机监听。

### 1. 启动服务

```bash
git clone https://github.com/cryotx/Posthorn.git
cd Posthorn/deploy/compose

(
  set -eC
  umask 077
  { cat .env.example; printf '\nPOSTHORN_API_KEY=%s\n' "$(openssl rand -hex 32)"; } > .env
)

sudo install -d -m 0700 -o 65532 -g 65532 ./data
docker compose up --build --wait
```

默认使用本机的 `8080` 端口。如果端口已被占用，在 `.env` 中修改 `POSTHORN_HTTP_PORT` 后再启动。

### 2. 预览一封邮件

```bash
base_url="http://$(docker compose port posthorn 8080)"
client_key="$(sed -n 's/^POSTHORN_API_KEY=//p' .env)"

curl --fail-with-body "$base_url/api/send" \
  -H "Authorization: Bearer $client_key" \
  -H 'Content-Type: application/json' \
  --data '{"subject":"Posthorn 测试","message":"你好，这是一封预览邮件。"}'
```

> [!TIP]
> 返回 `"status":"dry_run"` 表示预览成功。默认只渲染邮件，适合先验证接入。

## 接入与管理

- **启用真实投递。** 在私有 `.env` 中填写 `RESEND_API_KEY`、已验证的发件地址 `POSTHORN_FROM` 和默认收件地址 `POSTHORN_TO`，将 `POSTHORN_DRY_RUN` 设为 `false`，再运行 `docker compose up -d`。其他服务商的配置见[多服务商示例](examples/multi-provider.toml)。
- **打开管理后台。** 用 `openssl rand -hex 32` 生成单独的管理员令牌，填到私有 `.env` 的 `POSTHORN_ADMIN_TOKEN`，再运行 `docker compose up -d`。默认登录地址为 `http://127.0.0.1:8080/admin/login`。
- **接入应用。** 配置网页表单、服务端 API 或 SMTP。表单接入步骤见[上游表单指南](https://posthorn.dev/getting-started/quick-start/)，SMTP 配置见[上游 SMTP 指南](https://posthorn.dev/features/smtp-ingress/)。

## 文档与示例

| 入口 | 内容 |
| --- | --- |
| [Docker Compose 部署](deploy/compose/README.md) | 校验配置、开启真实投递、配置管理后台和备份数据库 |
| [调用 Posthorn](docs/calling-posthorn.md) | Python、JavaScript 和 curl 的 API 请求示例 |
| [多服务商](examples/multi-provider.toml) | 服务商池、故障转移和路由规则 |
| [多项目](examples/projects.toml) | 按应用划分密钥、发件地址和配额 |
| [配额与费用路由](examples/quota-cost-routing.toml) | 先用免费额度，再切换付费服务商 |
| [生产配置示例](examples/production.toml) | 持久化存储、鉴权和运行参数 |

## 从源码构建

安装 Go 1.25 或更新版本，在仓库根目录运行以下命令。

```bash
make build
./bin/posthorn version
```

## 来源与许可证

本版本源自 [craigmccaskill/posthorn](https://github.com/craigmccaskill/posthorn)，沿用 [Apache-2.0 许可证](LICENSE)。[posthorn.dev](https://posthorn.dev) 提供上游文档。
