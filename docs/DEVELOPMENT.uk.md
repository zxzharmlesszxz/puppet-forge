# Розробка та релізи

[Огляд проєкту](../README.uk.md) | [English version](DEVELOPMENT.md)

Локальна розробка, інтеграційні середовища, перевірки, CI та процес релізу.

## Структура HTML UI

UI рендериться на сервері через Go `html/template`. Шаблони й ресурси вбудовані
в бінарний файл; окремої збірки frontend немає.

- `internal/httpapi/templates/layout.gohtml` містить єдиний каркас документа.
  `manage-layout.gohtml` і `public-layout.gohtml` додають навігацію та ресурси
  відповідної групи сторінок; сторінка входу використовує каркас без навігації.
- Сторінка визначає title, вміст, необов'язкові заголовок і підзаголовок, CSS та
  скрипти. Кожна сторінка має окремий набір шаблонів, тому однойменні блоки
  різних сторінок не перезаписують один одного.
- Навігація, повідомлення, фільтри, пагінація з вибором кількості елементів,
  форми публікації та картки модулів є спільними компонентами.
- `internal/httpapi/assets/base.css` містить спільні змінні й елементи керування;
  `manage.css` і `public.css` задають стилі груп сторінок. Окремі CSS-файли
  сторінок містять лише специфічні правила та перевизначення. Компоненти
  керування використовують спільні класи `content-panel`, `list-panel`,
  `compact-fields` і `report-panel`; відступи та адаптивні відмінності
  залишаються у стилях відповідних сторінок.
- JavaScript зберігається в `internal/httpapi/assets/*.js`. Каркаси підключають
  спільні контролери один раз із `defer`; контролер розкривних блоків читає ключ
  зберігання з body замість підстановки Go-значень у JavaScript.
- `page-size-persistence.js` перевіряє налаштування та працює з cookie й
  localStorage; він завантажується перед `async-lists.js`. Усі поточні елементи
  пагінації працюють через асинхронні списки; другого контролера, який
  перезавантажує всю сторінку, немає. `clipboard.js` надає спільне копіювання
  та тимчасову індикацію результату на кнопках сторінок токенів і модулів.
- `/assets/` віддає вбудовані CSS і JavaScript без автентифікації, підтримує
  `GET`/`HEAD` та повертає `Cache-Control: no-cache` для перевірки актуальності
  після розгортання. Теги скриптів зберігають CSP nonce відповіді.

Спільну поведінку слід додавати до компонентів і контролерів, а не копіювати
шапки документів, стилі чи скрипти в нову сторінку. Тести шаблонів перевіряють
каркас і підключення ресурсів; браузерні тести перевіряють роботу контролерів.

## Локальний старт

```bash
go test ./...
go run ./cmd/server
```

Тести відповідності PostgreSQL запускаються окремо, щоб звичайний `go test ./...` не залежав від локального Docker/PostgreSQL. Для перевірки однакової поведінки SQLite і PostgreSQL задай DSN тестової бази:

```bash
make test-postgres
```

Інтеграційні тести об'єктних сховищ також запускаються окремо. Коли MinIO та fake-gcs-server з локального набору Compose запущені, виконай:

```bash
make test-object-storage
```

Об'єднаний target запускає контракти S3 та GCS. Вони перевіряють незмінні create-only завантаження, потокове повне й діапазонне читання, отримання списку та ідемпотентне видалення; контракт S3 додатково перевіряє конкурентний запис одного ключа й очищення після скасування. Для одного backend використовуй `make test-s3-storage` або `make test-gcs-storage`; адреси інших ізольованих емуляторів передаються через відповідні змінні `PUPPET_FORGE_TEST_*_ENDPOINT`.

`make coverage-check` вимагає щонайменше 65% загального unit coverage та окремі мінімальні значення для критичної бізнес-логіки. Коли доступні PostgreSQL, MinIO і fake-gcs-server, `make coverage-integration-check` запускає unit та integration suites, об'єднує їхні atomic coverage profiles без дублювання statements і вимагає 70% загального coverage, зберігаючи окремі мінімальні значення для пакетів. У CI профілі створюються в ізольованих jobs, після чого перевіряється їхній об'єднаний результат.

Для самодостатнього локального запуску `make coverage-integration-docker` піднімає одноразові контейнери PostgreSQL, MinIO, fake-gcs-server і Go runner в ізольованій Docker-мережі. Він використовує стандартні внутрішні порти сервісів, очікує їхньої готовності, запускає той самий об'єднаний gate і видаляє контейнери, анонімні volumes та мережу після успіху, помилки або переривання.

## Docker Compose

Для локального стенду:

```bash
docker compose up --build
```

Локальний набір відкриває:

- Puppet Forge на `http://localhost:8080`;
- Prometheus на `http://localhost:9091`;
- Grafana на `http://localhost:3001`, типово з обліковими даними `admin` / `admin`.

Grafana запускається з уже налаштованими джерелом даних Prometheus і дашбордом Puppet Forge. Порт можна перевизначити через `GRAFANA_PORT`, а локальні облікові дані через `GRAFANA_ADMIN_USER` та `GRAFANA_ADMIN_PASSWORD`.

Compose запускає:

- `app` з цим сервісом;
- `postgres`; сервіс сам створює потрібні таблиці при старті;
- `fake-gcs-server` як локальний емулятор GCS;
- `minio` і `minio-init` для локального S3-сумісного сховища;
- `r10k` контейнер на базі `puppet/r10k`, який автоматично запускає `puppetfile install` і завершує роботу.

Локальне сховище можна перемикати так:

- `ARTIFACT_BACKEND=gcs` використовує `fake-gcs-server` через `ARTIFACT_ENDPOINT=http://gcs:4443`
- `ARTIFACT_BACKEND=s3` використовує `MinIO` через `ARTIFACT_ENDPOINT=http://minio:9000`

Окрема перевірка `r10k` через Compose:

1. Підніми стенд:

    ```bash
    docker compose up --build
    ```

2. Переконайся, що потрібні модулі доступні локально. Для зовнішніх модулів запит r10k на маршрут `/v3/*` сам проіндексує метадані й закешує артефакт.

3. У `testdata/r10k/Puppetfile` тримай лише зовнішні модулі для димової перевірки. Локальні приватні модулі треба спершу опублікувати у Forge, інакше r10k очікувано отримає `module does not exist`.

4. Запусти одноразовий контейнер `r10k`:

    ```bash
    docker compose up r10k
    ```

Після завершення:

```bash
docker compose logs r10k
```

Базова конфігурація в `docker-compose.yml` звертається до локального Forge за адресою `http://app`, тобто за іменем сервісу контейнера `app` у мережі Docker Compose, і передає локальний `ADMIN_TOKEN` як `authorization_token`, щоб димова перевірка працювала у приватному режимі `PUBLIC_MODULE_ACCESS=false`. Одноразовий контейнер установлює модулі в `/tmp/r10k-modules`, тому перевірка не залежить від прав на основній машині або іменованого тому.

### Локальний OIDC

Compose читає параметри OIDC зі змінних середовища або `.env`; типовим лишається
`WEB_AUTH_MODE=none`, щоб credentials не зберігались у YAML. Початковий приклад є
в [`.env.example`](../.env.example). Для локального Authentik використовуй hostname,
який відкривається у браузері, наприклад `forge.127.0.0.1.nip.io`, і зареєструй
точний redirect URI:

```text
http://forge.127.0.0.1.nip.io:8080/auth/callback
```

Issuer також має бути доступний із контейнера `app`. Якщо identity provider
запущений на основній машині через Docker Desktop, використовуй
`host.docker.internal` для issuer host, але лишай browser-facing URL Forge у
redirect URI.

## Makefile

Для стандартних локальних команд є [`Makefile`](../Makefile):

```bash
make help
make check
make test-browser
make test-postgres
make test-object-storage
make coverage-check
make coverage-integration-check
make compose-up
make http-smoke
make docker-smoke
make compose-smoke
make r10k
make oidc-preflight
make helm-package
```

`make http-smoke` перевіряє вже запущений HTTP-сервіс: `/healthz`, `/readyz`, окремий сервер метрик, публічний HTML-каталог, форму входу за токеном, політику API читання для `PUBLIC_MODULE_ACCESS` та читання і вхід за адміністративним токеном. Для нестандартних URL або токена використовуй `SMOKE_BASE_URL`, `SMOKE_METRICS_URL`, `SMOKE_PUBLIC_MODULE_ACCESS` і `SMOKE_ADMIN_TOKEN`. `make docker-smoke` збирає фінальний image і перевіряє non-root UID, вбудовану версію, довідку та OCI-мітку версії. `make compose-smoke` запускає локальний набір Compose у фоновому режимі, виконує димові перевірки HTTP та одноразовий запуск r10k, а код завершення r10k повертає до Makefile.

За замовчуванням Makefile використовує `go`, `gofmt` і `golangci-lint` з `PATH`. Інструменти можна перевизначати через змінні:

```bash
GO=go GOLANGCI_LINT=golangci-lint make check
```

`GOSEC_VERSION` тимчасово фіксує upstream-коміт
`14ef84f6feccf40be7f5092fd2db119bf3f5f836` із `golang.org/x/tools v0.52.0`.
Останній реліз gosec із тегом, `v2.29.0`, використовує `v0.49.0` та не може
прочитати export data Go 1.27.2. Не вимикай перевірки безпеки; після появи
сумісного релізу заміни SHA на його тег. Помилки завантаження пакетів або SSA
означають провал сканування, навіть якщо підсумок показує нуль знахідок.

`make helm-lint` перевіряє нерелізний код чарту з development values matrix; ці
values не призначені для production-розгортання.

`make test-browser` запускає Go fixture для рендерингу на основній машині, а браузерні
тести виконує в зафіксованому офіційному Docker image Playwright. Залежності
встановлюються в тимчасовій файловій системі контейнера, тому локальні Node.js, npm і
Chromium не потрібні, а версії браузера й test runner лишаються узгодженими локально та в CI.

## GitHub Actions

Процес [`CI`](../.github/workflows/ci.yml) запускається для запитів на злиття та надсилання змін до будь-якої гілки. Він викликає повторно використовуваний процес [`Checks`](../.github/workflows/checks.yml), який перевіряє форматування, `go vet`, `golangci-lint`, поріг покриття, browser-регресії Playwright, сумісність із PostgreSQL, інтеграцію з об'єктним сховищем, конфігурацію Docker Compose, перевірку й пакування Helm, окремі та згенеровані Helm-правила Prometheus, димову перевірку архівів релізу, пробну збірку Docker-образу, тести перегонів даних, `govulncheck`, `gosec` і сканування файлової системи та образу через Trivy.

Процес [`Release`](../.github/workflows/release.yml) запускається тільки для тегів `v*.*.*`. Перед публікацією він викликає той самий повторно використовуваний процес перевірок, але без локальної перевірки конфігурації Docker Compose, а для тегу `v1.2.3` будує:

- файли GitHub Release з бінарними архівами в `dist/*.tar.gz`, `dist/checksums.txt` і CycloneDX SBOM у `dist/sbom.cdx.json`;
- підписаний багатоплатформний Docker-образ `ghcr.io/<owner>/<repo>:v1.2.3` з BuildKit SBOM/provenance attestations; `:latest` просувається з цієї версії лише після успішної публікації бінарного релізу, версійного образу та Helm chart;
- Helm-чарт `puppet-forge-1.2.3.tgz` із `appVersion: v1.2.3`;
- індекс репозиторію Helm у GitHub Pages через `helm/chart-releaser-action`. Чарт публікується через репозиторій Helm, а не як файл GitHub Release.

Процес спочатку перевіряє, що committed Helm `version` та `appVersion` відповідають тегу релізу. Потім він створює чернетку GitHub Release, публікує та підписує незмінну версію Docker image і публікує незмінений chart із tagged commit. Лише коли всі версійні компоненти успішні, процес просуває `:latest` і робить GitHub Release публічним.

Усі кроки публікації можна безпечно повторити для того самого тегу: GitHub Release редагується на місці, версійний image лишається адресованим за вмістом, а chart-releaser пропускає вже наявний пакет чарту. Не створюй тег повторно й не збирай під тією самою версією інші артефакти.

Перед публікацією онови `deploy/puppet-forge/Chart.yaml`: `version` має дорівнювати версії релізу без початкової `v`, а `appVersion` — повному тегу. Закоміть цю зміну, а потім запусти повний локальний release gate із тією самою канонічною версією. Gate сам піднімає ізольовані контейнери PostgreSQL, MinIO та fake-gcs-server для перевірки об'єднаного integration coverage і завжди видаляє їх після завершення:

```bash
make release-preflight VERSION=v1.2.3
```

Після успішної перевірки захищений release-таргет вимагає чисту робочу директорію в `main`, відхиляє наявний локальний або віддалений тег, надсилає `main`, звіряє віддалений коміт, створює анотований тег і надсилає його для запуску процесу Release:

```bash
make push-release VERSION=v1.2.3
```

Release workflow публікує індекс і пакет чарту до налаштованої гілки `gh-pages`.
Опублікований реліз можна перевірити так:

```bash
helm repo update puppet-forge
helm search repo puppet-forge/puppet-forge --versions
```

## Додаткова документація

- Схеми роботи одного та кількох екземплярів: [SCHEMA.uk.md](SCHEMA.uk.md)
- Метрики: [METRICS.uk.md](METRICS.uk.md)
- Архітектура: [ARCHITECTURE.uk.md](ARCHITECTURE.uk.md)
- Резервне копіювання та відновлення: [BACKUP_RESTORE.uk.md](BACKUP_RESTORE.uk.md)
- Grafana dashboard: [examples/grafana/puppet-forge-dashboard.json](../examples/grafana/puppet-forge-dashboard.json)
- Prometheus rules: [examples/prometheus/alerts/puppet-forge.yml](../examples/prometheus/alerts/puppet-forge.yml)
