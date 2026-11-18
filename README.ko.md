# Posthorn

[简体中文](README.md) · [English](README.en.md) · [日本語](README.ja.md) · 한국어 · [Español](README.es.md)

Posthorn은 직접 운영하는 애플리케이션의 트랜잭션 이메일을 한곳에서 발송하는 게이트웨이입니다. HTTP 양식, HTTP API, SMTP로 요청을 받아 Resend, Postmark, Mailgun, Amazon SES, SMTP 릴레이 등 설정한 발송 서비스로 전달합니다. 여러 서비스를 함께 사용하면 서비스에 장애가 발생하거나 발송 한도에 도달했을 때 다른 서비스로 발송을 시도할 수 있습니다.

## Docker로 발송 미리 보기

Docker Compose 예제는 소스에서 로컬 이미지를 빌드하고, 기본적으로 로컬 주소에만 바인딩하며, SQLite에 발송 기록을 저장합니다. 초기 설정에서는 메일을 실제로 발송하지 않고 요청 결과를 미리 볼 수 있습니다.

```bash
git clone https://github.com/cryotx/Posthorn.git
cd Posthorn/deploy/compose
```

Bash에서 클라이언트 API 키를 생성한 뒤 서비스를 시작합니다. 아래의 디렉터리 권한 설정은 기본 구성의 Linux Docker를 기준으로 합니다.

```bash
(
  set -C
  umask 077
  { cat .env.example; printf '\nPOSTHORN_API_KEY=%s\n' "$(openssl rand -hex 32)"; } > .env
)
sudo install -d -m 0700 -o 65532 -g 65532 ./data
docker compose up -d --build
```

`/api/send`에 요청하면 응답의 `status`가 `dry_run`으로 표시됩니다.

```bash
base_url="http://$(docker compose port posthorn 8080)"
client_key="$(sed -n 's/^POSTHORN_API_KEY=//p' .env)"
curl --fail-with-body "$base_url/api/send" \
  -H "Authorization: Bearer $client_key" \
  -H 'Content-Type: application/json' \
  --data '{"subject":"Posthorn 테스트","message":"안녕하세요."}'
```

실제 발송을 켜거나 데이터베이스를 백업하는 방법은 [Docker Compose 가이드](deploy/compose/README.md)를 참조하세요.

## 관리 화면과 프로젝트 키

별도의 관리자 토큰을 `.env`의 `POSTHORN_ADMIN_TOKEN`에 설정하고 서비스를 다시 구성한 다음, 서비스 주소의 `/admin/login`을 열어 로그인합니다. 관리 화면에서 한국어를 선택할 수 있습니다.

프로젝트별 API 키를 만들어 사용하면 애플리케이션마다 별도의 인증 정보를 관리할 수 있습니다. 화면에서 발급한 키는 한 번만 표시됩니다. 키를 교체하면 이전 키가 즉시 무효화되므로 해당 애플리케이션의 설정을 새 키로 업데이트하세요. 관리 화면에서 발송 서비스와 배달 기록, 재시도 대기열, 감사 로그도 확인할 수 있습니다.

## 소스에서 빌드

Go 1.25 이상을 사용합니다. 저장소 루트에서 다음 명령을 실행합니다.

```bash
mkdir -p bin
cd core
mkdir -p ../bin
go build -o ../bin/posthorn ./cmd/posthorn
../bin/posthorn version
```

## 문서와 라이선스

- [Docker Compose 배포 가이드](deploy/compose/README.md)
- [Python과 JavaScript에서 API 호출하기](docs/calling-posthorn.md)
- [업스트림 구성 및 배포 문서](https://posthorn.dev/)

이 버전은 [craigmccaskill/posthorn](https://github.com/craigmccaskill/posthorn)을 기반으로 하며, 원본과 같은 [Apache License 2.0](LICENSE)을 따릅니다.
