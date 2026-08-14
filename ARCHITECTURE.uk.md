# Архітектура

[English version](ARCHITECTURE.md)

> **Runtime-схеми та карта перевірок:** [SCHEMA.uk.md](SCHEMA.uk.md) показує
> топології одного й кількох екземплярів, послідовності запитів, координацію
> спільного стану, запобіжники та перевірки окремих вузлів.

## Огляд

`puppet-forge` — сервіс на Go, який зберігає внутрішні релізи Puppet-модулів,
надає невеликий HTML UI, API для публікації та видалення адміністраторами й
проксіює `/v3/*` до публічного Puppet Forge.

Сервіс розділяє:

- метадані модулів у SQL;
- артефакти релізів в об'єктному сховищі;
- проксі й кеш upstream Forge в окремому proxy layer.

## Структура пакетів

- `cmd/server` — точка входу процесу, завантаження конфігурації, налаштування
  logger і запуск HTTP-серверів.
- `internal/app` — збирання застосунку: storage backends, SQL store, auth,
  upstream proxy, фоновий refresh і router.
- `internal/httpapi` — HTTP-маршрути, JSON API handlers, HTML-сторінки,
  health/readiness і перевірки доступу.
- `internal/service` — нормалізація публікації, перевірка архіву, оркестрація
  upload, upstream indexing і читання релізів.
- `internal/store` — SQL-зберігання модулів, релізів, leases та access config;
  реалізації PostgreSQL і SQLite.
- `internal/storage` — GCS і S3-compatible backends для артефактів.
- `internal/proxy` — reverse proxy до API Puppet Forge та object-storage cache
  для завантажень.
- `internal/auth` — token-based API authorization для read, publish і
  admin-scoped delete.
- `internal/webauth` — необов'язкова OIDC session auth для HTML UI.
- `internal/observability` — метрики Prometheus і HTTP middleware logging.

## Потоки даних

### Публікація модуля

1. Клієнт надсилає `POST /api/v1/modules` із multipart-полями `space` і `file`.
2. `internal/httpapi` автентифікує principal і перевіряє право публікації у
   вибраний space.
3. `internal/service` перевіряє єдиний canonical root модуля, безпечні regular
   entries, обмежений розпакований розмір і рівно один `metadata.json` у root,
   після чого читає owner, name, version, description, README і metadata.
4. Сервіс перевіряє відповідність namespace/root архіву вибраному space та
   отриманій identity.
5. Multipart input, checksum, upload і відповіді з артефактами використовують
   streaming readers, тому розмір архіву не стає heap-витратами одного запиту.
6. Сервіс обчислює MD5, SHA-256 і розмір, а потім серіалізує публікацію цієї
   identity між репліками.
7. Сховище створює `<prefix>/<owner>/<name>/<version>/<sha256>.tar.gz` із
   backend precondition, що не дозволяє перезаписати наявний об'єкт.
8. SQL створює локальний реліз лише за відсутності module/version. Повтор тих
   самих bytes ідемпотентний; інші bytes для тієї самої версії повертають
   `409 Conflict` і не замінюють реліз.
9. Однозначна SQL-помилка видаляє незакомічений object і порожній module row.
   Якщо стан commit неможливо визначити, content-addressed object зберігається
   для reconciliation, щоб не видалити вже закомічені дані.
10. API повертає release payload. Ручні identity/metadata overrides відхиляються.

### Читання

1. Клієнт запитує metadata модуля або релізу через `/api/v1/modules/...` чи UI.
2. SQL store повертає records модуля й релізу.
3. Для локальних релізів URL завантаження будується з налаштованого artifact
   backend.
4. Для upstream-релізів сервіс може доповнити відсутні поля через upstream
   proxy integration.

### Upstream proxy

1. Клієнт звертається до `/v3/*` цього сервісу.
2. Proxy передає запит публічному Puppet Forge.
3. JSON-відповіді GET і HEAD кешуються в пам'яті на
   `UPSTREAM_PROXY_JSON_CACHE_TTL`.
4. Якщо upstream недоступний після завершення TTL, stale JSON може повертатися
   лише в межах `UPSTREAM_PROXY_JSON_STALE_TTL`.
5. Cold body для `/v3/files/*` передається потоково безпосередньо з upstream у
   create-only object під `upstream-cache/`. Розмір і `Content-Length`
   перевіряються під час цього потоку; лише після завершення запису сервіс
   відкриває object і потоково віддає його клієнту. Отже, артефакт не зберігається
   в request-sized byte slice у пам'яті, а клієнт не може побачити частковий
   cache object.
6. Свіжі metadata upstream-модулів індексуються локально для вебінтерфейсу та метрик. JSON
   cache hit лише оновлює стан використання релізу й не повторює індексацію
   module/releases. Усі локальні та upstream-шляхи використання об'єднують SQL-записи
   до одного на реліз за хвилину на кожній репліці через обмежений in-memory throttle
   на 10 000 ключів; спільна SQL лишається джерелом active-release стану.
7. Для cold module response observer та SQL indexing завершуються до віддачі
   response. Observer від'єднує client cancellation, зберігає request values і
   має власний timeout 30 секунд, тому наступний запит r10k не може побачити
   частково індексовані releases або скасувати indexing operation.

### Фоновий refresh

1. Якщо задано `UPSTREAM_SYNC_INTERVAL`, застосунок запускає refresh loop.
2. Процес отримує lease у SQL, щоб одночасно не було кількох refresh leaders.
3. Репліка-лідер оновлює кешовані upstream-модулі із заданим інтервалом через обмежений пул воркерів `UPSTREAM_SYNC_CONCURRENCY`.

## Модель зберігання

Сервіс використовує два persistence layers:

- SQL для модулів, релізів, access/session state і leases;
- object storage для архівів модулів і кешованих upstream-файлів.

SQL backends:

- PostgreSQL через `postgres://...`;
- SQLite через `sqlite:///...`.

Artifact backends:

- Google Cloud Storage;
- S3-compatible storage.

GCS client і створені URL артефактів використовують налаштований HTTP(S)
`ARTIFACT_ENDPOINT` без зміни глобального emulator state процесу. Незмінний
create-only контракт S3 backend вимагає, щоб вибраний сервіс атомарно виконував
`If-None-Match: *` для `PutObject`; підтримувані backends перевіряє integration
test об'єктного сховища.

Видалення локального релізу атомарно прибирає SQL metadata й записує
content-addressed object path у надійний `artifact_deletions` outbox. Обраний
через lease worker щохвилини повторює idempotent видалення object. Перед цим він
бере module lock і перевіряє чинне SQL-посилання, тому повторна публікація того
самого immutable path скасовує застаріле завдання, а не видаляє живий вміст.
Upstream-релізи можуть посилатися на спільний `upstream-cache/`; видалення їхніх
metadata не додає цей спільний object до outbox. Окремий щоденний worker, обраний
через lease, видаляє лише cache objects без SQL-посилань, старші за
`UPSTREAM_ARTIFACT_ORPHAN_TTL`.

Періодичні воркери refresh та cleanup є критичними компонентами процесу.
Очікувані операційні помилки логуються і повторюються, але неочікуваний panic
воркера не перехоплюється локально: процес завершується, щоб runtime перезапустив
повністю ініціалізований екземпляр замість Ready API з назавжди зупиненим cleanup.

## Модель маршрутів

Основні групи:

- `/api/v1/modules` — публікація та список модулів;
- `/api/v1/modules/{owner}/{name}` — читання й видалення модуля;
- `/api/v1/modules/{owner}/{name}/versions/{version}` — читання й видалення релізу;
- `/api/v1/modules/{owner}/{name}/versions/{version}/download` — завантаження;
- `/` — HTML-каталог;
- `/modules/...` — HTML-сторінки модуля й витягнутих файлів;
- `/v3/*` — upstream Forge proxy;
- `/metrics` — Prometheus endpoint на окремому `METRICS_ADDR`, не на listener
  застосунку;
- `/healthz` — liveness процесу;
- `/readyz` — readiness залежностей;
- `/manage` — автентифікований read-only overview модулів і керованих команд;
- `/manage/modules` — cross-team publish, search, delete й upstream imports;
- `/manage/teams` і `/manage/teams/{team}/{access,tokens,modules}` — список команд
  та окремі team-scoped сторінки доступу, токенів і модулів;
- `/manage/admin/{access,spaces}` — глобальна конфігурація доступу та publish
  spaces лише для global admins.

Повна таблиця методів і дій міститься в [SCHEMA.uk.md](SCHEMA.uk.md).

## Модель доступу

API auth використовує access config у SQL. JSON seed path немає: база є source
of truth для teams, tokens, publish spaces та OIDC mappings. SQLite і PostgreSQL
створюють operational schema кодом під час старту.

- read token дозволяє read API, downloads і `/v3/*`;
- publish token дозволяє read/publish/update у налаштованих spaces;
- raw token після створення не зберігається й не повертається: SQL містить ID,
  prefix, HMAC-SHA256 digest з `ACCESS_TOKEN_PEPPER`, назву, роль і timestamps;
- успішна auth оновлює `last_used_at` не частіше разу на хвилину на репліку;
  SQL додатково відхиляє застарілі timestamp updates. Цей telemetry throttle
  обмежений 10 000 останніх token ID на репліку й за більшої cardinality видаляє
  найстаріший запис; він не бере участі в authorization;
- legacy plaintext tokens мігруються після налаштування pepper;
- Manage auth використовує opaque cookie IDs і SQL sessions. Token session
  містить HMAC ID, credential digest/ID, CSRF, timestamps і revocation;
  OIDC session — mapped identity claims та ті самі lifecycle/CSRF дані;
- OIDC state індексується HMAC, живе п'ять хвилин і споживається атомарно один
  раз між репліками. Flow використовує nonce і PKCE S256, callback суворо
  перевіряє signature, issuer, audience, algorithm, expiry та nonce;
- окремий `MANAGE_SESSION_SECRET` захищає token-session IDs,
  `ACCESS_TOKEN_PEPPER` використовується лише для access-token digests, а
  `OIDC_COOKIE_SECRET` захищає transient state cookie й OIDC session IDs.
  Кожне значення має бути стабільним та однаковим на всіх репліках;
- рання request boundary прибирає forwarded headers від недовірених peers.
  Forwarded host/proto/client приймаються лише після opt-in, CIDR, syntax і
  conflict checks; `ALLOWED_PUBLIC_HOSTS` обмежує effective hosts;
- global admins можуть видаляти в усіх spaces, team admins — лише у primary
  space керованої команди. Extra publish spaces не дають delete ownership;
- `ADMIN_TOKEN` — runtime-only bootstrap/break-glass credential, не записується
  в SQL;
- інформаційний каталог `/` і `/modules/...` публічний незалежно від
  `PUBLIC_MODULE_ACCESS`;
- `PUBLIC_MODULE_ACCESS=true` обходить лише read auth для install/API routes;
- `PUBLIC_MODULE_ACCESS=false` вимагає read/publish/admin auth для цих routes.

OIDC UI вмикається через `WEB_AUTH_MODE=oidc`. `oidc_groups` дають team publish,
`oidc_team_admin_emails` та `oidc_team_admin_groups` — delegated team admin, а
`oidc_admin_groups`, `oidc_admin_emails`, `oidc_admin_subjects` — global admin.
Team admin редагує лише власні токени й OIDC mappings та видаляє лише у primary
space. `platform-admin` зарезервовано для global-admin configuration і не може
бути publishing team або module namespace.

Якщо одна OIDC identity збігається з кількома mappings, усі capabilities і team
scopes об'єднуються. Global admin не скасовує team admin чи publishing rights,
але global mapping сам по собі не дає publish rights.

`/manage/teams` показує доступні principal команди й підсумок spaces, modules,
tokens та OIDC mappings. Access, Tokens і Modules розділені на окремі сторінки;
backend authorization незалежно перевіряє кожну операцію.

Публічні й Manage-списки з пагінацією використовують єдиний progressive-enhancement
контракт. Сервер завжди формує канонічний HTML списку, а спільний browser-контролер
запитує потрібну сторінку та замінює лише відповідний блок разом із рядками,
лічильниками, `Per page` і посиланнями Previous/Next. Фільтри, незалежні списки на
одній сторінці та browser Back/Forward працюють через той самий механізм; звичайна
навігація лишається fallback у разі відсутності JavaScript або помилки fragment-запиту.
Розмір сторінки зберігається в same-site cookie й browser storage та підтримує 10, 20,
50 або 100 рядків. Manage-каталоги модулів застосовують owner/search-фільтрацію на
рівні SQL, а release/activity завантажуються лише для поточної сторінки одним bounded
batch query.

`/manage/admin/access` керує global OIDC access, `/manage/admin/spaces` —
publish-space assignments. Повного HTTP replace access config немає. Global
admins створюють teams, керують extra spaces, global mappings і всіма teams.
Team admins керують власними token lifecycle та дозволеними OIDC mappings.
Підроблений team update не може змінити space assignments.

Token creation вимагає назву та роль `read`/`publish`. Активна пара name/role
унікальна в team, а immutable token ID є lifecycle identity. Raw value показується
один раз у `no-store` response. Active та inactive lists незалежно пагінуються,
початково по 20 записів, із запам'ятовуваним вибором 10, 20, 50 або 100;
`ACCESS_TOKEN_HISTORY_TTL` визначає retention revoked/expired records. Cleanup
асинхронно запускається під спільною SQL-lease одразу після старту й щодня, тому
readiness не очікує завершення retention. Primary team space створюється автоматично й
не може бути unassigned. Extra spaces змінює лише global administration.
Bearer authorization отримує capabilities із bounded authorizer snapshot, а
потім на кожному protected request перевіряє immutable token ID безпосередньо в
SQL. Тому revoke та expiration закривають доступ на всіх replicas одразу після
database commit без повного перечитування всіх teams.

Latest releases та releases, активні в межах `ACTIVE_RELEASE_TTL`, не можна
видалити. Module delete відхиляється, якщо є protected releases. Retention
release usage запускається асинхронно під спільною SQL lease
`release-usage-cleanup` одразу після старту й щодня, тому read paths не виконують
cleanup writes. SQL-позначки
видалених upstream releases блокують фоновий refresh протягом
`DELETED_RELEASE_TTL`, але прямий запит точної версії відновлює її на вимогу для
старих клієнтів. Cleanup позначок асинхронно запускається під спільною SQL-lease
одразу після старту й щодня, а `0` зберігає позначки нерозшуканих релізів
безстроково. Якщо в SQL немає
жодного effective credential, startup вимагає `ADMIN_TOKEN` для початкового
налаштування через `/manage/teams`, `/manage/admin/spaces` і
`/manage/admin/access`.

## Поведінка при відмовах

Periodic upstream refresh — singleton job із lease у спільній таблиці
`app_leases`. Інша replica отримує lease після expiry або release. Cycle має
bounded context та idempotent upserts, тому partial cycle можна повторити.

Ліміти OIDC login/callback, token login і publish зберігаються в SQL та спільні
для всіх реплік. Вони fail closed, якщо shared limiter недоступний. Ліміти
search, download і v3 reads залишаються обмеженими та process-local. Expiry
обробляється bounded priority queue без повного сканування всіх client buckets
на кожному запиті. При заповненні витісняється bucket із найближчим reset, тому
хвиля нових клієнтів не блокує раніше невідомі адреси назавжди. Ingress/API
gateway має бути додатковою межею для adversarial traffic; read-ліміти не є
strict cluster-wide quota.

- SQL initialization failure не дозволяє сервісу стартувати;
- artifact storage initialization failure не дозволяє старт;
- upstream proxy initialization failure не дозволяє старт;
- невалідний publish archive повертає `400`;
- відсутня або недостатня auth повертає `401` чи `403`;
- `/healthz` показує liveness процесу;
- `/readyz` перевіряє SQL метаданих і доступ до object storage через module service.

## Спостережуваність

Сервіс експортує structured HTTP logs, Prometheus HTTP/operation/inventory
metrics. Канонічний контракт метрик описано в [METRICS.uk.md](METRICS.uk.md).
