# Puppet Forge Service

Сервіс на Go для внутрішнього аналога Puppet Forge:

- публікація модулів і версій;
- керування командними publish spaces;
- token та OIDC доступ для команд;
- proxy/cache офіційного Puppet Forge API;
- зберігання артефактів у GCS або S3-compatible backend;
- зберігання метаданих у SQL backend;
- запуск у Kubernetes.

## Можливості

Поточна реалізація покриває:

- `POST /api/v1/modules` для завантаження нової версії модуля;
- `GET /api/v1/modules?limit=20&offset=0` для списку модулів; відповідь містить `items`, `limit`, `offset` і `total`;
- `GET /api/v1/modules/{owner}/{name}` для картки модуля;
- `DELETE /api/v1/modules/{owner}/{name}` для видалення модуля через admin/team-admin доступ;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}` для конкретного релізу;
- `DELETE /api/v1/modules/{owner}/{name}/versions/{version}` для видалення окремої версії через admin/team-admin доступ;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}/download` для редіректу на об'єкт у GCS;
- `GET /` для HTML index-сторінки зі списком локально опублікованих модулів;
- `GET /modules/{owner}/{name}` для HTML-сторінки модуля з markdown README, dropdown вибору версії та інструкціями встановлення;
- `GET /v3/*` і `HEAD /v3/*` для reverse proxy на офіційний Puppet Forge API;
- `/manage` для публікації, видалення й імпорту upstream-модулів через web UI;
- `/manage/access` для DB-backed access config;
- `ADMIN_TOKEN` для bootstrap/break-glass доступу;
- OIDC login для global admins, team admins і командних publish-груп;
- `PUBLIC_MODULE_ACCESS` для режиму публічного machine-readable/install доступу;
- active release tracking, який забороняє backend delete для latest/in-use версій і ховає недоступні delete actions у `/manage`;
- `GET /healthz` і `GET /readyz`.

Поза поточною реалізацією:

- пошук із фільтрами, рейтингами, залежностями й verified publishers;
- повна API-сумісність з офіційним Puppet Forge;
- перевірка вмісту архіву Puppet-модуля;
- CDN і signed URLs з TTL.

## Архітектура

- HTTP API: стандартний `net/http`;
- Service layer: валідація, оркестрація завантаження та запису метаданих;
- Artifact storage: GCS bucket;
- Metadata store: `postgres://` або `sqlite://` backend, який обирається через `DATABASE_DSN`.
- Upstream proxy: reverse proxy на офіційний Forge API з in-memory TTL cache для JSON GET/HEAD відповідей.

Потік публікації:

1. Клієнт надсилає multipart-запит із tar.gz артефактом і метаданими.
2. Сервіс валідує owner/name/version.
3. Файл завантажується в GCS у префікс `modules/<owner>/<name>/<version>/`.
4. Метадані релізу записуються в обраний SQL backend.
5. API повертає опис створеного релізу.

Потік проксіювання:

1. Клієнт звертається до `/v3/...` на нашому сервісі.
2. Сервіс проксіює запит до офіційного Forge API.
3. JSON-відповіді кешуються в пам'яті на `UPSTREAM_PROXY_JSON_CACHE_TTL`.
4. Завантаження файлів через `/v3/files/...` при першому запиті зберігаються в GCS під префіксом `upstream-cache/`, а далі сам сервіс віддає cached object з GCS без redirect.

## Конфігурація

Див. [`.env.example`](./.env.example).

## Локальний старт

```bash
go test ./...
go run ./cmd/server
```

PostgreSQL parity tests are opt-in, щоб звичайний `go test ./...` не залежав від локального Docker/Postgres. Для перевірки однакової поведінки SQLite і PostgreSQL задай DSN тестової бази:

```bash
make test-postgres
```

## Як опублікувати свій модуль

Сервіс приймає `tar.gz` архів Puppet-модуля через `POST /api/v1/modules` як `multipart/form-data`.

Мінімальні вимоги до модуля:

- у корені модуля має бути валідний `metadata.json`;
- `metadata.json.name` має бути у форматі `<owner>-<name>`, наприклад `teamname-apache`;
- версія береться з `metadata.json.version`, якщо її не передати окремо в form fields;
- README бажано покласти в `README.md`, тоді він з'явиться на HTML-сторінці модуля.

Приклад структури:

```text
teamname-apache/
  metadata.json
  README.md
  manifests/
    init.pp
```

Приклад `metadata.json`:

```json
{
  "name": "teamname-apache",
  "version": "1.2.3",
  "summary": "Apache module",
  "author": "Example Team"
}
```

Зібрати архів можна стандартним `puppet module build`:

```bash
cd /path/to/teamname-apache
puppet module build
```

Після цього Puppet зазвичай покладе артефакт у `pkg/teamname-apache-1.2.3.tar.gz`.

Рекомендований варіант: сервіс сам вичитає `owner`, `name`, `version` і `description` з архіву.

```bash
export FORGE_URL="https://forge.example.com"
export PUBLISH_TOKEN="replace-me"

curl -X POST "${FORGE_URL}/api/v1/modules" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}" \
  -F "file=@pkg/teamname-apache-1.2.3.tar.gz" \
  -F 'metadata={"source":"internal-ci"}'
```

Важливо:

- для звичайної публікації достатньо передати тільки `file`;
- `owner`, `name`, `version`, `description` сервіс дістане з архіву;
- якщо передати ці поля у form fields, вони перекриють значення з архіву;
- токен має бути в `publish_tokens` і мати право на потрібний publish space. У structured UI space з назвою `Team` додається автоматично; додаткові spaces додаються через `Extra publish spaces`.

Ручний варіант: можна явно передати form fields, якщо треба перевизначити значення з архіву або дописати службові metadata.

```bash
curl -X POST "${FORGE_URL}/api/v1/modules" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}" \
  -F "file=@pkg/teamname-apache-1.2.3.tar.gz" \
  -F "owner=teamname" \
  -F "name=apache" \
  -F "version=1.2.3" \
  -F "description=Apache module" \
  -F 'metadata={"source":"internal-ci","git_sha":"abc123"}'
```

Швидка перевірка після публікації:

```bash
curl "${FORGE_URL}/api/v1/modules/teamname/apache"
curl -I "${FORGE_URL}/api/v1/modules/teamname/apache/versions/1.2.3/download"
```

HTML-сторінка модуля буде доступна за адресою:

```text
https://forge.example.com/modules/teamname/apache
```

UI зараз також включає:

- live filter на index-сторінці `/`;
- швидкий перехід до upstream module page з index;
- markdown rendering для README на сторінці модуля;
- dropdown вибору версії модуля;
- готові інструкції встановлення модуля і підключення цього сервісу як custom Forge.

Observability:

- HTTP logging для всіх запитів;
- Prometheus metrics на `GET /metrics`.
- Інвентарні метрики для модулів:
  - `puppet_forge_module_info{module,owner,name,latest_version} 1`
  - `puppet_forge_module_releases{source}`
  - `puppet_forge_module_latest_releases{source}`
- Операційні метрики:
  - `puppet_forge_publish_total{result,owner}`
  - `puppet_forge_delete_total{result,kind,owner}`
  - `puppet_forge_release_usage_mark_total{result,owner}`
  - `puppet_forge_upstream_sync_total{result,trigger}`
  - `puppet_forge_upstream_refresh_cycles_total{result}`
  - `puppet_forge_upstream_refresh_duration_seconds`
  - `puppet_forge_upstream_refresh_last_duration_seconds`
  - `puppet_forge_upstream_refresh_last_success_timestamp_seconds`
  - `puppet_forge_upstream_refresh_last_error_timestamp_seconds`
  - `puppet_forge_upstream_refresh_modules{result}`
  - `puppet_forge_upstream_cache_requests_total{kind,result}`

Metrics:

- `puppet_forge_module_info` експортує по одному ряду на кожен локально доступний модуль;
- `module` зараз має формат `<owner>-<name>`;
- `owner` це namespace або publisher модуля;
- `name` це коротка назва модуля без owner prefix;
- `latest_version` це остання версія, відома локальному forge для цього модуля.
- `puppet_forge_module_releases` експортує кількість локально доступних релізів, згруповану по `source`;
- `puppet_forge_module_latest_releases` експортує кількість latest-релізів, згруповану по `source`;
- release-level inventory метрики навмисно агреговані, щоб `/metrics` залишався швидким навіть при великій кількості версій модулів.

Форма `puppet_forge_module_info` навмисно узгоджена з метрикою `puppetfile_module_info` з `prometheus-puppetfile-exporter`, щоб у Grafana і PromQL можна було напряму порівнювати:

- `current_version` з `Puppetfile`;
- `latest_version` з локального Forge;
- спільні labels `owner` і `name`.

Це дозволяє будувати запити на кшталт "що зараз pinned у Puppetfile, але вже відстає від останньої версії у внутрішньому forge".

Повний опис метрик див. у [METRICS.md](./METRICS.md).

Runtime parameters can be passed either through environment variables or command-line flags. Environment variables are still supported, and command-line flags override environment values when both are set.

The binary prints its build version and exits with:

```bash
puppet-forge --version
```

Flag names are the lowercase kebab-case form of the environment variable name:

```text
DATABASE_DSN -> --database-dsn
MANAGE_SESSION_SECRET -> --manage-session-secret
PUBLIC_MODULE_ACCESS -> --public-module-access
UPSTREAM_PROXY_JSON_CACHE_TTL -> --upstream-proxy-json-cache-ttl
UPSTREAM_PROXY_JSON_STALE_TTL -> --upstream-proxy-json-stale-ttl
```

Example:

```bash
puppet-forge \
  --database-dsn "postgres://forge:forge@postgres:5432/forge?sslmode=disable" \
  --admin-token "replace-me-bootstrap-token" \
  --artifact-backend s3 \
  --artifact-endpoint "http://minio:9000" \
  --artifact-bucket "puppet-forge-artifacts" \
  --public-module-access=false
```

Environment variables:

| Змінна | За замовчуванням | Обов'язкова | Опис |
| --- | --- | --- | --- |
| `APP_ENV` | `dev` | ні | Назва runtime-оточення для логів і діагностики. |
| `HTTP_ADDR` | `:8080` | ні | Адреса, на якій HTTP server слухає запити всередині контейнера або процесу. |
| `READ_TIMEOUT` | `10s` | ні | Максимальний час читання HTTP request. Формат Go duration, наприклад `10s`, `1m`. |
| `WRITE_TIMEOUT` | `30s` | ні | Максимальний час запису HTTP response. |
| `SHUTDOWN_TIMEOUT` | `10s` | ні | Graceful shutdown timeout. |
| `DATABASE_DSN` | порожньо | так | DSN metadata store. `postgres://...` вмикає PostgreSQL, `sqlite:///path/file.db` вмикає SQLite. |
| `ADMIN_TOKEN` | порожньо | для порожньої БД | Runtime bootstrap/break-glass admin token. Не зберігається в БД; потрібен, щоб на чистому старті зайти в `/manage/access`. |
| `MANAGE_SESSION_SECRET` | порожньо | ні | Shared secret для encrypted `/manage` token sessions. Задай його для multi-replica deployments. Якщо порожній, сервіс використовує fallback: `OIDC_COOKIE_SECRET`, потім `ADMIN_TOKEN`, потім per-process random secret. |
| `ARTIFACT_BACKEND` | `gcs` | ні | Backend для tarball артефактів: `gcs` або `s3`. |
| `ARTIFACT_ENDPOINT` | `https://storage.googleapis.com` | для `s3`; опційно для `gcs` | Endpoint object storage. Для `gcs` можна вказати emulator/custom host; для `s3` це S3-compatible endpoint, наприклад MinIO. |
| `ARTIFACT_BUCKET` | порожньо | так | Bucket/container для module tarballs і upstream artifact cache. |
| `ARTIFACT_PROJECT` | порожньо | для `gcs` | GCP project для GCS bucket operations. Для `s3` не використовується. |
| `ARTIFACT_PREFIX` | `modules` | ні | Prefix усередині bucket для локально опублікованих модулів. |
| `ARTIFACT_REGION` | `us-east-1` | ні | Region для S3-compatible клієнта. |
| `ARTIFACT_ACCESS_KEY_ID` | порожньо | для приватного `s3` | Access key для S3-compatible storage. |
| `ARTIFACT_SECRET_ACCESS_KEY` | порожньо | для приватного `s3` | Secret key для S3-compatible storage. |
| `ARTIFACT_PATH_STYLE` | `true` | ні | Вмикає path-style S3 URLs. Корисно для MinIO, GCS interoperability і локальних endpoint-ів. |
| `PUBLIC_BASE_URL` | порожньо | ні | Optional fallback для побудови absolute URLs. Якщо порожній, сервіс бере URL із `Host`, `X-Forwarded-*` або `Forwarded` headers поточного request. |
| `PUBLIC_MODULE_ACCESS` | `false` | ні | Якщо `true`, API читання, downloads і `/v3/*` відкриті без token. Якщо `false`, ці install/API routes потребують read/publish/admin token. HTML-каталог `/` і `/modules/...` лишається інформаційно доступним. Publish/delete/manage завжди закриті. |
| `ACTIVE_RELEASE_TTL` | `720h` | ні | Скільки часу release вважається active/in-use після запиту r10k або `puppet module install`; active/latest версії не можна видалити через API або `/manage`. |
| `SECURITY_HSTS_ENABLED` | `false` | ні | Вмикає response header `Strict-Transport-Security`. Для локального HTTP лишай вимкненим; вмикай лише коли публічний endpoint завжди HTTPS. |
| `WEB_AUTH_MODE` | `none` | ні | Web auth режим: `none` або `oidc`. Token auth для API працює незалежно від цього. |
| `OIDC_ISSUER_URL` | порожньо | для `WEB_AUTH_MODE=oidc` | OIDC issuer discovery URL, наприклад Authentik application provider URL. |
| `OIDC_CLIENT_ID` | порожньо | для `WEB_AUTH_MODE=oidc` | OIDC client id. |
| `OIDC_CLIENT_SECRET` | порожньо | для `WEB_AUTH_MODE=oidc` | OIDC client secret. |
| `OIDC_REDIRECT_URL` | порожньо | ні | Explicit callback URL. Якщо порожній, callback будується з поточного request base URL як `/auth/callback`; для multi-ingress це рекомендований режим. |
| `OIDC_LOGOUT_URL` | auto-discovery/порожньо | ні | Provider end-session URL. Якщо не задано, сервіс пробує взяти `end_session_endpoint` із OIDC discovery. |
| `OIDC_COOKIE_SECRET` | порожньо | для `WEB_AUTH_MODE=oidc` | Secret для підпису web session/state cookies. Має бути стабільним між рестартами pod-ів. |
| `UPSTREAM_URL` | `https://forgeapi.puppetlabs.com` | ні | Upstream Puppet Forge API, куди proxy ходить для модулів, яких немає локально. |
| `UPSTREAM_PROXY_JSON_CACHE_TTL` | `5m` | ні | TTL in-memory cache для JSON GET/HEAD відповідей upstream proxy (`/v3/modules/...`, `/v3/releases/...`). Не керує object-storage cache tarball-ів із `/v3/files/...`. `0s` фактично вимикає JSON response cache. |
| `UPSTREAM_PROXY_JSON_STALE_TTL` | `1h` | ні | Максимальний час після завершення `UPSTREAM_PROXY_JSON_CACHE_TTL`, протягом якого proxy може віддати stale JSON cache, якщо upstream Forge повернув помилку або недоступний. `0s` вимикає stale fallback. |
| `FORGE_CACHE_MAX_BODY_BYTES` | `1048576` | ні | Максимальний розмір upstream JSON response body, який можна покласти в in-memory proxy cache. |
| `MODULE_UPLOAD_MAX_BYTES` | `134217728` | ні | Максимальний розмір publish upload request у байтах. Перевищення повертає `413 Request Entity Too Large` і не читається повністю в пам'ять. |
| `UPSTREAM_ARTIFACT_MAX_BYTES` | `134217728` | ні | Максимальний розмір upstream tarball-артефакту з `/v3/files/...`, який proxy дозволяє завантажити (як для object-storage cache, так і для bypass-шляху без artifact storage). Перевищення повертає `413 Request Entity Too Large`. |
| `UPSTREAM_SYNC_INTERVAL` | `0s` | ні | Інтервал background refresh уже закешованих upstream-модулів. `0s` вимикає фоновий refresh. |
| `UPSTREAM_SYNC_LIMIT` | `1000` | ні | Максимальна кількість upstream-модулів, які refresh cycle обробляє за один запуск. |
| `METRICS_MODULE_LIMIT` | `10000` | ні | Максимальна кількість модулів, які inventory metrics експортують за один прохід збору. |

Artifact storage backends:

- `ARTIFACT_BACKEND=gcs` використовує Google Cloud Storage;
- `ARTIFACT_ENDPOINT` за замовчуванням дорівнює `https://storage.googleapis.com`; якщо перевизначити, сервіс піде в цей endpoint як у кастомний GCS host/emulator або S3-compatible endpoint;
- `ARTIFACT_BUCKET` задає bucket для артефактів;
- `ARTIFACT_PROJECT` задає GCP project для bucket operations у `gcs` backend;
- `ARTIFACT_PREFIX` задає storage prefix для модульних артефактів;
- `ARTIFACT_BACKEND=s3` використовує S3-compatible object storage, включно з GCS interoperability, MinIO або Ceph;
- для `s3` треба задати `ARTIFACT_ENDPOINT`, `ARTIFACT_BUCKET`, `ARTIFACT_ACCESS_KEY_ID`, `ARTIFACT_SECRET_ACCESS_KEY`; `ARTIFACT_PATH_STYLE=true` корисний для GCS/MinIO-style endpoint-ів;
- `UPSTREAM_SYNC_INTERVAL` вмикає фонове оновлення вже закешованих upstream-модулів з офіційного Forge;
- `UPSTREAM_SYNC_LIMIT` обмежує кількість upstream-модулів на один цикл оновлення.

Database backend:

- `DATABASE_DSN=postgres://...` вмикає PostgreSQL backend;
- `DATABASE_DSN=sqlite:///data/puppet-forge.db` вмикає SQLite backend;
- вибір database backend іде за схемою DSN, окремий `DB_BACKEND` не потрібен.
- PostgreSQL є production-oriented backend для multi-replica deployment. SQLite підходить для local і single-writer deployment; refresh lease там спирається на SQLite transaction locking, а не на PostgreSQL `FOR UPDATE NOWAIT`.

Access control:

- конфігурація доступів зберігається тільки в БД: `access_teams`, `access_tokens`, `access_publish_owners`, `access_oidc_mappings`;
- `ACCESS_JSON` більше не використовується;
- `ADMIN_TOKEN` це runtime bootstrap/break-glass токен, який не пишеться в БД;
- якщо БД порожня і `ADMIN_TOKEN` не заданий, сервіс не стартує;
- на чистому старті увійди в `/manage` через `ADMIN_TOKEN`, відкрий `/manage/access` і налаштуй teams, tokens, OIDC groups та OIDC team admins;
- веб-інтерфейс (`/` і `/modules/...`) лишається відкритим для інформаційного перегляду локальних модулів незалежно від `PUBLIC_MODULE_ACCESS`;
- `read_tokens` дають доступ до API читання, download і `/v3/*`;
- `publish_tokens` дають доступ до читання й публікації/оновлення лише в дозволені publish spaces, але не дають права видаляти модулі;
- видалення модулів і версій доступне global admins у будь-якому namespace і OIDC team admins тільки у primary space своєї team; `Extra publish spaces` дозволяють publish/update, але не дають delete ownership;
- OIDC team-admin mappings дають доступ до редагування tokens і OIDC groups тільки своєї team;
- OIDC admin mappings дають глобальний доступ до `/manage/access` і видалення модулів/версій у будь-якому namespace;
- `PUBLIC_MODULE_ACCESS=false` вимагає read/publish/admin token для install/API routes: API читання, release download, extracted module files і `/v3/*`;
- `PUBLIC_MODULE_ACCESS=true` відкриває ці install/API routes без токена, щоб r10k і `puppet module install` могли забирати модулі без Authorization header;
- `PUBLIC_MODULE_ACCESS=true` не відкриває publish, delete, `/manage` або `/manage/access`.

Web auth:

- `WEB_AUTH_MODE=oidc` вмикає session login для веб-інтерфейсу через OIDC/Authenik;
- API при цьому лишається на token auth для команд;
- потрібні `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_COOKIE_SECRET`;
- `OIDC_REDIRECT_URL` можна задати явно, але зазвичай краще лишити порожнім: сервіс побудує redirect URL із поточного request host/proto і додасть `/auth/callback`;
- `PUBLIC_BASE_URL` не обовʼязковий і використовується лише як fallback, якщо request не містить `Host`/`Forwarded`/`X-Forwarded-*`;
- для Kubernetes ingress має передавати реальний `Host` і scheme через `X-Forwarded-Host`/`X-Forwarded-Proto` або RFC `Forwarded`;
- для командного web UI OIDC-користувачі мапляться на team через `oidc_groups`;
- для delegated team admin доступу OIDC-користувачі мапляться через `oidc_team_admin_emails` або `oidc_team_admin_groups`.

Командний web UI:

- `/manage` відкриває web-сторінку для командного керування модулями;
- `/manage/access` доступний global admin-користувачам для повного керування access config;
- team admin-користувачі також можуть відкривати `/manage/access`, але бачать тільки свою team і можуть редагувати тільки свої `read_tokens`, `publish_tokens`, `oidc_groups`, `oidc_team_admin_emails` і `oidc_team_admin_groups`;
- team admin-користувачі не можуть перейменовувати team, видаляти teams, змінювати `Extra publish spaces`, редагувати global admins, відкривати JSON editor або змінювати чужі teams;
- `/manage/access` має structured form для типових командних полів і advanced JSON editor для повної заміни конфігурації, доступний тільки global admins;
- головна сторінка показує `Manage`;
- якщо `WEB_AUTH_MODE=oidc`, `/manage` використовує OIDC login і права з БД;
- без OIDC або як fallback можна увійти через існуючий `publish_token` з БД або через runtime `ADMIN_TOKEN`;
- OIDC identity або publish token бачить і змінює тільки publish space з назвою `Team` і додаткові spaces з `Extra publish spaces`;
- publish token не може видаляти модулі або версії;
- OIDC team admin може видаляти модулі й версії тільки у своїх publish spaces;
- `ADMIN_TOKEN` або OIDC admin mapping можуть видаляти модулі й версії в будь-якому namespace;
- версії, які завантажували r10k або `puppet module install` протягом `ACTIVE_RELEASE_TTL`, позначаються як `in use`, і кнопка видалення для них у `/manage` не показується;
- `ACTIVE_RELEASE_TTL` за замовчуванням дорівнює `720h` / 30 днів і задається при старті сервісу;
- видалені upstream-версії записуються в tombstone-таблицю `deleted_releases`, тому наступний upstream sync не створює їх знову;
- видалення всього модуля очищає tombstones і release usage для цього module, тому після повторного upstream sync модуль можна створити заново з усіма доступними upstream-релізами;
- upload форми використовує ті самі правила, що й `POST /api/v1/modules`: owner/name/version можна взяти з `metadata.json` архіву або перевизначити form fields.

Для `docker-compose.yml` OIDC можна ввімкнути через env:

```bash
export WEB_AUTH_MODE=oidc
export PUBLIC_MODULE_ACCESS="false"
export OIDC_ISSUER_URL="https://auth.example.com/application/o/puppet-forge/"
export OIDC_CLIENT_ID="puppet-forge"
export OIDC_CLIENT_SECRET="replace-me"
export OIDC_COOKIE_SECRET="32-byte-random-secret"
# Optional: якщо discovery не віддає end_session_endpoint.
export OIDC_LOGOUT_URL="https://auth.example.com/application/o/puppet-forge/end-session/"
export ADMIN_TOKEN="replace-me-bootstrap-token"
docker compose up
```

У локальному [`docker-compose.yml`](./docker-compose.yml) ці значення беруться з env або `.env`, а runtime також підтримує відповідні CLI flags. Приклад без секретів лежить у [`.env.example`](./.env.example). За замовчуванням Compose стартує з `WEB_AUTH_MODE=none`, щоб випадково не тримати OIDC secrets у YAML.

Для локального Authentik часто краще не використовувати `localhost` у redirect URI. Задай локальний DNS hostname, який відкривається в браузері, наприклад:

```bash
open "http://forge.127.0.0.1.nip.io:8080"
```

У Authentik provider треба додати exact redirect URI:

```text
http://forge.127.0.0.1.nip.io:8080/auth/callback
```

Якщо сервіс доступний через кілька ingress hostnames і `OIDC_REDIRECT_URL` порожній, треба додати callback для кожного hostname, наприклад:

```text
https://forge.example.com/auth/callback
https://forge.dev.example.com/auth/callback
```

Якщо Authentik запущений у Docker або на host machine, `OIDC_ISSUER_URL` також має бути доступний із контейнера `app`. Для Authentik на host machine з Docker Desktop це зазвичай hostname `host.docker.internal`, але redirect URI все одно має бути URL сервісу Forge, який відкриває браузер.

Logout із `/manage` чистить локальні token/OIDC cookies. Якщо OIDC discovery віддає `end_session_endpoint`, або якщо задано `OIDC_LOGOUT_URL`, сервіс також відправляє браузер у provider logout, щоб Authentik не залогінив користувача назад silent login-ом зі старої provider-сесії.

Для групи користувачів краще мапити не emails, а OIDC group:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"]
}
```

У Authentik треба додати користувачів у групу `teamname-devops` і переконатися, що application/provider віддає claim `groups` в ID token. Для командного доступу в UI використовується саме `oidc_groups`: одна OIDC-група дає команді publish-доступ до publish space з назвою `Team`. Якщо треба дозволити ще й інші spaces, додай їх у `Extra publish spaces`. Після login, якщо mapping не спрацює, сервіс залогує `email`, `subject`, `domain` і `groups` у повідомленні `oidc session is not mapped to team`.

Щоб команда сама менеджила свої токени й OIDC-групи, додай `OIDC team admins`. Для одного-двох людей зручніше email-и; коли список росте, краще перейти на групу:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"],
  "oidc_team_admin_emails": ["owner@example.com"],
  "oidc_team_admin_groups": ["teamname-admins"]
}
```

Користувач із email `owner@example.com` або з групи `teamname-admins` може зайти в `/manage/access`, але побачить тільки `teamname` і не зможе змінити інші teams, global admins, JSON config або extra publish spaces. У `/manage` такий team admin може видаляти модулі й версії тільки в primary space своєї team. `Extra publish spaces` лишаються publish/update scope, а видаляти там може тільки global admin.

Один OIDC team-admin email або group можна додати в кілька teams. Тоді користувач побачить і зможе редагувати всі ці teams у `/manage/access`, але все одно не матиме доступу до чужих teams або global admin settings.

Адмінський OIDC-доступ задається у блоці `Global OIDC Admins`, щоб не змішувати team publish-групи з глобальними правами:

```json
{
  "team": "platform-admin",
  "oidc_admin_groups": ["forge-admins"]
}
```

Користувач із групи `forge-admins` може відкривати `/manage/access` і видаляти модулі й версії в будь-якому namespace. Publish через admin group не вмикається автоматично.

Якщо один OIDC-користувач одночасно входить у team group, наприклад `teamname-devops`, і має admin mapping через `oidc_admin_groups`, `oidc_admin_emails` або `oidc_admin_subjects`, admin mapping має пріоритет. Це дозволяє додати себе як admin через email, не прибираючи себе з командної групи.

Щоб додати себе як admin після першого запуску:

1. Увійди в `/manage` через `ADMIN_TOKEN`, наприклад локальний `forge-admin-token-local`.
2. Відкрий `/manage/access`.
3. У `Global OIDC Admins` додай свій email у `OIDC admin emails` або свою групу в `OIDC admin groups`.
4. Збережи global admins і перелогінься через OIDC.

`ADMIN_TOKEN` варто лишати тільки для bootstrap/break-glass, наприклад коли OIDC зламаний. Для щоденного доступу використовуй `OIDC admin groups` або `OIDC admin emails`.

Приклад DB-backed access config, який можна внести через `/manage/access`:

```json
[
  {
    "team": "teamname",
    "read_tokens": ["teamname-read-token"],
    "publish_tokens": ["teamname-publish-token"],
    "oidc_groups": ["teamname-devops"],
    "oidc_team_admin_emails": ["owner@example.com"],
    "oidc_team_admin_groups": ["teamname-admins"]
  },
  {
    "team": "carbon",
    "read_tokens": ["carbon-read-token"],
    "publish_tokens": [],
    "oidc_groups": ["carbon-devops"]
  },
  {
    "team": "platform-admin",
    "oidc_admin_groups": ["forge-admins"]
  }
]
```

Helm chart також підтримує ресурси Prometheus Operator, але за замовчуванням вони вимкнені:

- `serviceMonitor.enabled=true` створює `ServiceMonitor`;
- `prometheusRule.enabled=true` створює `PrometheusRule` із тими ж recording і alert rules, що й [examples/prometheus/alerts/puppet-forge.yml](./examples/prometheus/alerts/puppet-forge.yml).

## Docker Compose

Для локального стенду:

```bash
docker compose up --build
```

Сервіс буде доступний на `http://localhost:8080`.

Compose підіймає:

- `app` з цим сервісом;
- `postgres`; сервіс сам створює потрібні таблиці при старті;
- `fake-gcs-server` як локальний GCS emulator.
- `minio` і `minio-init` для локального S3-compatible storage backend.
- `r10k` контейнер на базі `puppet/r10k`, який автоматично запускає `puppetfile install` і завершує роботу.

Локальний storage backend можна перемикати так:

- `ARTIFACT_BACKEND=gcs` використовує `fake-gcs-server` через `ARTIFACT_ENDPOINT=http://gcs:4443`
- `ARTIFACT_BACKEND=s3` використовує `MinIO` через `ARTIFACT_ENDPOINT=http://minio:9000`

Standalone `r10k` перевірка з compose:

1. Підніми стенд:

```bash
docker compose up --build
```

2. Переконайся, що потрібні модулі доступні локально. Для upstream-модулів r10k-запит до `/v3/*` сам проіндексує metadata і закешує артефакт.

3. У `testdata/r10k/Puppetfile` тримай upstream-only модулі для smoke-перевірки. Локальні приватні модулі треба спершу опублікувати в Forge, інакше r10k очікувано отримає "module does not exist".

4. Запусти `r10k` one-shot контейнер:

```bash
docker compose up r10k
```

Після завершення:

```bash
docker compose logs r10k
```

Базова конфігурація в `docker-compose.yml` ходить у локальний Forge по `http://app`, тобто за service name контейнера `app` у Docker Compose network, і передає локальний `ADMIN_TOKEN` як `authorization_token`, щоб smoke працював у приватному режимі `PUBLIC_MODULE_ACCESS=false`. One-shot контейнер ставить модулі в `/tmp/r10k-modules`, щоб smoke не залежав від прав на host або named volume.

## Makefile

Для стандартних локальних команд є [`Makefile`](./Makefile):

```bash
make help
make check
make test-postgres
make compose-up
make http-smoke
make compose-smoke
make r10k
make oidc-preflight
make helm-package
```

`make http-smoke` перевіряє вже запущений HTTP instance: `/healthz`, `/readyz`, `/metrics`, публічний HTML-каталог, token login form, read API policy для `PUBLIC_MODULE_ACCESS` і admin-token read/login. Для нестандартного URL або токена використовуй `SMOKE_BASE_URL`, `SMOKE_PUBLIC_MODULE_ACCESS` і `SMOKE_ADMIN_TOKEN`. `make compose-smoke` піднімає локальний Compose stack у detached-режимі, запускає HTTP smoke і r10k one-shot, а r10k exit code повертається назад у make.

За замовчуванням Makefile використовує `go`, `gofmt` і `golangci-lint` з `PATH`. Інструменти можна перевизначати через змінні:

```bash
GO=go GOLANGCI_LINT=golangci-lint make check
```

## Kubernetes

Helm chart лежить у [`deploy/puppet-forge`](./deploy/puppet-forge).

Приклад:

```bash
helm upgrade --install puppet-forge ./deploy/puppet-forge
```

Prometheus Operator ресурси вимкнені за замовчуванням. Для scrape і alerts увімкни `serviceMonitor.enabled=true` та `prometheusRule.enabled=true`.

## GitHub Actions

Workflow [`CI`](./.github/workflows/ci.yml) запускається для pull request і push у будь-яку branch. Він викликає reusable workflow [`Checks`](./.github/workflows/checks.yml), який перевіряє форматування, `go vet`, `golangci-lint`, coverage threshold, Docker Compose config, Helm lint/package, release archive smoke, Docker image dry-build і race tests.

Workflow [`Release`](./.github/workflows/release.yml) запускається тільки для тегів `v*.*.*`. Перед публікацією він викликає той самий reusable checks workflow, але без локальної Docker Compose config перевірки, а для тегу `v1.2.3` будує:

- GitHub Release assets з binary archives у `dist/*.tar.gz` і `dist/checksums.txt`;
- multi-arch Docker image `ghcr.io/<owner>/<repo>:v1.2.3` і `:latest`;
- Helm chart `puppet-forge-1.2.3.tgz` із `appVersion: v1.2.3`;
- Helm repository index у GitHub Pages через `helm/chart-releaser-action`. Chart публікується через Helm repo, а не як GitHub Release asset.

Для Helm repo потрібно мати branch `gh-pages` і GitHub Pages, налаштований на цей branch. Після релізу chart можна підключати як:

```bash
helm repo add puppet-forge https://<owner>.github.io/<repo>
helm repo update
helm upgrade --install puppet-forge puppet-forge/puppet-forge
```

## Додаткова документація

- Метрики: [METRICS.md](./METRICS.md)
- Архітектура: [ARCHITECTURE.md](./ARCHITECTURE.md)
- Grafana dashboard: [examples/grafana/puppet-forge-dashboard.json](./examples/grafana/puppet-forge-dashboard.json)
- Prometheus rules: [examples/prometheus/alerts/puppet-forge.yml](./examples/prometheus/alerts/puppet-forge.yml)
