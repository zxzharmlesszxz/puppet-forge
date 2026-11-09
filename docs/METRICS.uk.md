# Метрики

[English version](METRICS.md)

> **Runtime-контекст:** [SCHEMA.uk.md](SCHEMA.uk.md) пояснює, які метрики є
> локальними для репліки, які gauge описують спільний стан і як правильно
> агрегувати їх у multi-instance розгортанні.

## Стани, Доступ та Аудит

### `puppet_forge_resources`

- Кешований gauge із мітками `kind` (`module`, `release`), `source` (`local`,
  `upstream`, `unknown`) та `state` (`available`, `pending`, `deprecated`, `blocked`, `unknown`).
- Рахує наявні записи каталогу, включно з прихованими від публічного читання.
  Видалені записи та збережені блокування без даних каталогу не враховуються.
- Модуль має власний стан модерації. Для релізу враховано також обмеження
  батьківського модуля: спочатку Blocked, потім Pending, потім Deprecated,
  інакше Available. Джерело модуля визначається зі збереженого походження;
  якщо історичні джерела перетинаються, пріоритет має local.
- `latest` та спостережуване використання незалежні від модерації й не створюють
  окремих станів. Deprecated доступний для читання; Pending і Blocked — ні.
- Після успішного refresh порожні комбінації мають нуль. Невідомі значення міток
  згортаються до `unknown`; мітки owner/module/version не додаються.
- Використовує чинні `METRICS_REFRESH_INTERVAL`, readiness, контроль свіжості
  та збереження останнього успішного snapshot. SQL-агрегація виконується під час
  refresh, а не scrape. Старі інвентарні метрики не змінюються.
- Спільні gauge дедуплікуємо між репліками; локальні counter підсумовуємо:

```promql
sum by (kind, state) (
  max by (job, kind, source, state) (puppet_forge_resources)
)
```

### `puppet_forge_resource_oldest_pending_timestamp_seconds`

Кешований gauge із `kind` та `source`: Unix timestamp найстарішого Pending
з точністю до секунди; нуль означає порожню чергу погодження. Для релізів,
обмежених Pending батьківського модуля, береться timestamp цього модуля.
Між репліками використовуємо мінімальний ненульовий timestamp і перевіряємо
поточну кількість Pending. Це вік стану, а не час останнього скачування.
Не можна віднімати нуль від `time()` як timestamp очікування.

### `puppet_forge_control_operations_total`

Counter з обмеженими `operation`, `kind`, `result`. Операції: `approve`,
`deprecate`, `restore`, `block`, `unblock`, `clear_cache`, `delete`,
`force_delete`, `review`, `compare`, `review_download`; інші значення — `other`.
Результати: `success`, `rejected`, `error`.

Рахує завершені виклики service, а не запити, відхилені до цього виклику.
Відмови через права, валідацію, стан, конфлікт, відсутній ресурс, ліміти review
чи скасування клієнтом — `rejected`; збої SQL/storage і таймаути — `error`.
Успішна мутація рахується після commit. Review вимірює перевірку архіву,
не подальше підсвічування HTML чи передачу відповіді; `review_download` —
відкриття Pending-архіву для перевірки адміністратором. Публікації надалі
обліковуються чинною `puppet_forge_publish_total`.

Успішний Clear cache означає від'єднання шляхів і commit завдань очищення,
а не фізичне видалення об'єктів. Реальні результати worker показують
`puppet_forge_artifact_deletion_total` та `puppet_forge_artifact_deletions_pending`.
Спільний об'єкт або об'єкт із новим посиланням може бути свідомо збережений.

### `puppet_forge_authorization_checks_total`

Counter з обмеженими `scope` і `result` (`success`, `rejected`, `error`). Scope:
`read`, `publish`, `delete`, `manage`, `token_active` та shared rate-limit
перевірки з префіксом `rate_limit_`: `publish`, `manage-login`, `manage-publish`,
`auth-login`, `auth-callback`. Невідомий scope — `other`.
Це рішення окремих перевірок, не кількість унікальних запитів: автентифікація
токена й перевірка його активності в SQL обліковуються окремо. Публічне читання
та успішне OIDC-читання враховуються; необов'язкове визначення токена на
публічному запиті не створює ще одну перевірку.
Звичайні відмови й вичерпаний rate limit не є внутрішніми помилками.
Збій backend не відкриває доступ. HTTP-метрики зберігають 401/403/429 та інші
відмови конкретних handler: не кожна локальна перевірка прав має додатковий counter.

### `puppet_forge_access_config_refresh_total`

Counter із `result=success|error`: лише реальні спроби читання SQL і побудови
конфігурації доступів. TTL cache hits, об'єднані запити й очікування lock без
refresh не рахуються. Метрика показує збої навіть тоді, коли придатний кеш
авторизації ще дозволяє обслуговувати запити.

### `puppet_forge_audit_writes_total`

Counter із `result=success|error`: окремі спроби запису аудиту та транзакційний
запис разом із commit мутації. Rollback або збій commit не є успіхом; логування
handler після успішного транзакційного запису не додає другий запис метрики.
Це не кількість історичних рядків аудиту. Нетранзакційний login/security event
може відбутися навіть при невдалому записі аудиту; тоді подробиці шукайте в логах.

### `puppet_forge_security_events_total`

Counter з обмеженими `action` та `result=success|failure`: події аудиту входу й
виходу, змін команд/доступів/токенів, публікації й заміни, модерації, review,
видалення, upstream import та Clear cache. Невідомі значення — `other`.
Події рахуються незалежно від успіху запису аудиту. Актори, токени та їхні ID,
email, IP, причини й diff лишаються в аудиті, не в цих мітках. Це не повний
лічильник усіх відхилених HTTP-запитів.

### Алерти та Dashboard

Standalone rules та Helm `PrometheusRule` містять дедуплікований між репліками
інвентар, інформаційне нагадування про Pending старше 24 годин (і ще 30 хвилин
очікування алерту), внутрішні збої адміністративних операцій і авторизації,
сталі відмови доступу, помилки аудиту, refresh конфігурації та невідомі стани.
Сама наявність Available, Deprecated чи Blocked не означає аварію сервісу.

Helm-алерти target/scrape автоматично використовують job metrics Service чарти,
включно з `nameOverride` та `fullnameOverride`. `prometheusRule.targetJobRegex`
потрібний лише для іншої назви job у зовнішньому scrape-конфігу. Standalone
правила типово мають `job=~".*puppet-forge.*"`; адаптуйте цей selector до своїх
jobs. Grafana знаходить jobs за HTTP-метрикою Forge й розгортає **All** у
знайдені назви: слово `forge` у назві не обов'язкове.

Пороги Helm: `prometheusRule.thresholds.pendingApprovalMaxAgeSeconds` (86400),
`authorizationRejectionMinimumRate` (1 перевірка/с),
`authorizationRejectionRatio` (0.2). Обидва пороги відмов мають перевищуватися
10 хвилин. Збій запису аудиту — critical; звичайна відмова валідації чи прав
не спричиняє алерт внутрішніх помилок адміністративних операцій.

Вкладка Grafana `Lifecycle & Access` показує стани, вік погодження, результати
адміністративних операцій, Clear cache, примусові видалення, перевірки доступу,
аудит, refresh конфігурації, security events і свіжість інвентарю. Кольори станів:
зелений/жовтий/фіолетовий/червоний для Available/Pending/Deprecated/Blocked.
Поточний інвентар і legacy consumers читаються з instant gauge, а не історичного
`last_over_time`, який повертав би вже видалені набори міток. Counter показують
активність за обраний діапазон часу. Затримка refresh лишається; репліка зі
застарілим snapshot тимчасово може показувати старі числа, тому свіжість
потрібно перевіряти разом з інвентарем.

## Метрика збірки

### `puppet_forge_build_info`

- Тип: gauge зі сталим значенням `1` для поточної збірки
- Мітки:
  - `version`: версія застосунку, задана під час збірки
  - `go_version`: версія Go runtime

## HTTP-метрики

### `puppet_forge_http_requests_total`

- Тип: counter
- Значення: загальна кількість оброблених HTTP-запитів
- Мітки:
  - `method`
  - `route`
  - `status`

### `puppet_forge_http_request_duration_seconds`

- Тип: histogram
- Значення: тривалість HTTP-запиту в секундах
- Мітки:
  - `method`
  - `route`
  - `status`
- Примітка: використовує стандартні buckets Prometheus

### `puppet_forge_http_panics_total`

- Тип: counter
- Значення: загальна кількість panic у HTTP-handler, які middleware перехопив
- Мітки:
  - `method`
  - `route`
- Примітки:
  - panic до запису заголовків відповіді також фіксується в
    `puppet_forge_http_requests_total` зі статусом `500`;
  - panic після запису заголовків уже не може змінити відправлений клієнту HTTP-
    статус, але все одно збільшує цей counter.

### `puppet_forge_http_in_flight_requests`

- Тип: gauge
- Значення: поточна кількість HTTP-запитів у процесі обробки
- Мітки: немає

Нормалізовані значення `route`:

- `/`
- `/healthz`
- `/readyz`
- `/manage`
- `/manage/*`
- `/auth/*`
- `/api/v1/modules`
- `/api/v1/modules/`
- `/api/v1/modules/*`
- `/modules/*`
- `/v3/files/*`
- `/v3/*`
- `other`

## Метрики інвентарю

### `puppet_forge_modules`

- Тип: gauge
- Значення: кількість локально проіндексованих модулів власника
- Мітки:
  - `owner`
- Примітка: назви окремих модулів і версії не експортуються, щоб не створювати
  series із високою кардинальністю

### `puppet_forge_module_releases`

- Тип: gauge
- Значення: кількість локально проіндексованих релізів за джерелом
- Мітки:
  - `source`
- Примітка: метрика навмисно агрегована, щоб розмір `/metrics` і тривалість
  scrape залишалися обмеженими

### `puppet_forge_module_latest_releases`

- Тип: gauge
- Значення: кількість останніх відомих релізів модулів за джерелом
- Мітки:
  - `source`

### Стан збирача інвентарних метрик

Збирач інвентарю оновлює обмежений знімок у фоновому режимі. Prometheus читає лише
цей знімок із пам'яті, тому scrape не очікує завершення SQL-запитів.

- `puppet_forge_module_metrics_ready`: `1` після першого успішного оновлення, інакше `0`
- `puppet_forge_module_metrics_last_success_timestamp_seconds`: Unix-час останнього успішного оновлення
- `puppet_forge_module_metrics_refresh_errors_total`: кількість невдалих фонових оновлень
- `puppet_forge_module_metrics_owners_total`: кількість власників до застосування `METRICS_MODULE_LIMIT`
- `puppet_forge_module_metrics_truncated`: `1`, якщо ряди з міткою власника обрізано налаштованим лімітом

Кожен процес сервісу підтримує власний знімок. Для діагностики застарілої або
несправної репліки використовуй стандартну мітку Prometheus `instance`.

## Метрики операцій

### `puppet_forge_publish_total`

- Тип: counter
- Значення: загальна кількість спроб публікації, оброблених service layer
- Мітки:
  - `result`: `success` або `error`

### `puppet_forge_delete_total`

- Тип: counter
- Значення: загальна кількість спроб видалення
- Мітки:
  - `result`: `success` або `error`
  - `kind`: `module` або `release`

### `puppet_forge_release_usage_mark_total`

- Тип: counter
- Значення: загальна кількість спроб позначити використання релізу
- Мітки:
  - `result`: `success` або `error`

Позначка використання записується, коли клієнт запитує конкретний реліз або
завантажує його архів через API чи сумісні маршрути `/v3/releases/*` і
`/v3/files/*`. Читання metadata модуля не позначає його latest-реліз активним.
Manage UI використовує ті самі дані, щоб приховувати видалення релізів, які використовуються.
Повторні спостереження об'єднуються до одного SQL-запису на реліз за хвилину на
кожній репліці, тому counter рахує спроби запису до store, а не кожен HTTP-запит.

### Споживачі релізів

- `puppet_forge_release_consumer_last_seen_timestamp_seconds{consumer_team,consumer,consumer_role,owner,module,version,latest_version,status}` містить час останнього запиту конкретного архіву, який вдалося пов'язати з іменованим збереженим access-токеном.
- `puppet_forge_release_consumer_observations{consumer_team,consumer,consumer_role,owner,module,version,latest_version,status}` показує збережені спостереження, об'єднані не частіше одного запису на хвилину на кожній репліці. Це gauge зі спільної SQL-бази, а не точний лічильник HTTP-запитів.
- `puppet_forge_release_consumer_mark_total{result}` рахує спроби запису до SQL після throttle.
- `puppet_forge_release_consumer_series_total` показує загальну кількість збережених записів до застосування ліміту експорту.
- `puppet_forge_release_consumer_metrics_truncated` дорівнює `1`, якщо `METRICS_RELEASE_CONSUMER_LIMIT` не дозволив додати старіші записи до snapshot.

Мітка `status` має значення `latest`, `legacy` або `unknown`, якщо останній реліз модуля вже невідомий. Експортується лише остання спостережена версія для кожного іменованого токена й модуля, тому завантаження latest-релізу прибирає попередній legacy-сигнал, а повторне завантаження старого релізу повертає його. Ця модель передбачає, що назви токенів ідентифікують окремі проєкти або середовища. `consumer_team` означає команду-власника токена, `consumer` містить задану оператором назву токена, а `consumer_role` має значення `read` або `publish`. Значення токенів, хеші, префікси, OIDC subjects та email ніколи не експортуються. Анонімний public access, OIDC, bootstrap-admin, неіменовані й legacy plaintext credentials навмисно не отримують атрибуцію. Валідний іменований bearer-токен усе одно враховується, коли `PUBLIC_MODULE_ACCESS=true`; невалідний токен не блокує інакше публічний доступ. Записи зберігаються протягом `RELEASE_CONSUMER_TTL` (типово 180 днів); collector оновлює snapshot у пам'яті й не звертається до SQL під час Prometheus scrape.

Вкладка Grafana `Legacy Usage` показує ці записи разом із використаною та latest-версіями. Ad hoc змінна `Filters` дозволяє фільтрувати за `consumer_team`, `consumer`, `consumer_role`, `owner`, `module` або `version`; унікальна назва токена дає його команді прямий покажчик на проєкт чи середовище без додавання окремої project identity до Forge.

Глобальні адміністратори можуть переглядати поточні legacy-сигнали на сторінці `/manage/admin/legacy`. Читачі й адміністратори команд мають аналогічну сторінку `/manage/teams/<team>/legacy`, обмежену токенами їхньої команди. Кожна сторінка безпосередньо зі збереженої SQL-історії вибирає останню спостережену версію для кожного іменованого токена й модуля, використовує серверну фільтрацію та пагінацію і не обмежується значенням `METRICS_RELEASE_CONSUMER_LIMIT`, яке визначає лише максимальний розмір Prometheus snapshot.

`legacy` означає відмінність від поточного доступного latest, а не обов'язково
меншу semver-версію. Блокування новішого релізу може пересунути latest назад.
Це спостереження, а не доказ того, що проєкт досі розгортає цю версію.

### `puppet_forge_artifact_deletion_total`

- Тип: counter
- Значення: спроби обробки надійного видалення артефактів, включно з очищеними upstream-копіями
- Мітки:
  - `result`: `deleted`, `canceled` або `error`

`canceled` означає, що до cleanup той самий content-addressed path знову став
частиною опублікованого релізу. Worker прибрав застаріле завдання outbox, не
видаляючи чинний об'єкт.
`error` залишає завдання в черзі та відкладає наступну спробу на одну хвилину,
щоб один проблемний об'єкт не блокував наступні записи.

### `puppet_forge_artifact_deletions_pending`

- Тип: gauge
- Значення: кількість артефактів у спільному SQL outbox, включно з очищеними upstream-копіями
- Агрегація для кількох реплік: `max by (job)`, оскільки всі репліки читають ту саму надійну чергу

### `puppet_forge_artifact_stream_total`

- Тип: counter
- Значення: завершені потоки відповідей з артефактами, включно з помилками після надсилання HTTP-заголовків
- Мітки:
  - `source`: `local` або `upstream_cache`
  - `result`: `success`, `client_cancel` або `error`

HTTP-запит уже може мати статус `200` або `206`, коли object storage чи з'єднання
клієнта переривається під час передавання. Для виявлення обрізаних архівів і
ймовірних checksum failures потрібно використовувати цей counter, а не лише
метрики HTTP-статусів.
Оповіщення `PuppetForgeArtifactStreamErrors` враховує лише `result="error"`;
скасування клієнтом залишаються на dashboard, але не викликають оповіщення.

### `puppet_forge_artifact_stream_bytes_total`

- Тип: counter
- Значення: кількість успішно записаних байтів відповідей з артефактами, згрупована за `source`
- Мітки:
  - `source`: `local` або `upstream_cache`

## Метрики upstream

### `puppet_forge_upstream_sync_total`

- Тип: counter
- Значення: загальна кількість спроб синхронізації upstream-модуля. Фоновий
  refresh збільшує counter один раз на кожен модуль, який намагається
  синхронізувати, а не один раз на весь цикл.
- Мітки:
  - `result`: `success` або `error`
  - `trigger`: `single` або `refresh`

### `puppet_forge_upstream_refresh_cycles_total`

- Тип: counter
- Значення: загальна кількість циклів upstream refresh
- Мітки:
  - `result`: `success` або `error`

### `puppet_forge_upstream_refresh_duration_seconds`

- Тип: histogram
- Значення: тривалість циклів upstream refresh у секундах
- Примітка: використовує стандартні buckets Prometheus

### `puppet_forge_upstream_refresh_enabled`

- Тип: gauge; `1`, якщо на репліці налаштовано фоновий upstream refresh,
  `0`, якщо вимкнено (`UPSTREAM_SYNC_INTERVAL=0s`). Це конфігурація, а не
  поточне володіння lease чи доказ успішного циклу.
- Алерт зависання враховує лише ввімкнені репліки й найбільший час завершення
  серед них. Здоровий обраний worker не спричиняє дублікатів алерту через
  неактивних followers; вимкнені репліки не приховують збій увімкненого worker.
  Вимкнені workers не спричиняють алерт. Максимальний вік має перевищувати
  налаштований інтервал refresh.

### `puppet_forge_upstream_refresh_last_duration_seconds`

- Тип: gauge
- Значення: тривалість останнього циклу upstream refresh у секундах

### `puppet_forge_upstream_refresh_last_timestamp_seconds`

- Тип: gauge
- Значення: Unix timestamp останнього завершеного циклу upstream refresh незалежно від його результату
- Використання з кількома репліками: duration і module-result gauges беруться з репліки з найбільшим timestamp. Для алертів worker-health спочатку застосовується `puppet_forge_upstream_refresh_enabled == 1`, потім перевіряється нульовий або застарілий timestamp.

### `puppet_forge_upstream_refresh_last_success_timestamp_seconds`

- Тип: gauge
- Значення: Unix timestamp останнього циклу upstream refresh, який завершився
  без помилок окремих модулів

### `puppet_forge_upstream_refresh_last_error_timestamp_seconds`

- Тип: gauge
- Значення: Unix timestamp останнього циклу upstream refresh, у якому була хоча
  б одна помилка окремого модуля

### `puppet_forge_upstream_refresh_modules`

- Тип: gauge
- Значення: кількість модулів останнього циклу upstream refresh
- Мітки:
  - `result`: `attempted`, `success` або `error`

### `puppet_forge_upstream_cache_requests_total`

- Тип: counter
- Значення: загальна кількість рішень upstream cache
- Мітки:
  - `kind`: `json` або `artifact`
  - `result`: `hit`, `miss`, `stale` або `bypass`
- Примітка: JSON `stale` фіксується лише тоді, коли кешована відповідь уже
  прострочена, але ще перебуває в межах `UPSTREAM_PROXY_JSON_STALE_TTL`

### `puppet_forge_upstream_artifact_integrity_total`

- Тип: counter
- Значення: результати об'єднаних перевірок цілісності кешу upstream-артефактів
- Мітки:
  - `result`: `valid`, `repaired` або `error`
- Примітки:
  - `valid` означає, що кешований об'єкт уже відповідав очікуваним checksum і розміру;
  - `repaired` означає, що відсутній або пошкоджений об'єкт було завантажено, після
    чого він успішно пройшов повторну перевірку;
  - паралельні перевірки одного об'єкта на одній репліці об'єднуються та рахуються один раз.

### `puppet_forge_upstream_artifact_cleanup_total`

- Тип: counter
- Значення: результати циклів очищення кешу upstream-артефактів
- Мітки:
  - `result`: `success` або `error`
- Примітка: результат cleanup записує лише worker, обраний через lease.

### `puppet_forge_upstream_artifact_cleanup_objects_total`

- Тип: counter
- Значення: кількість видалених об'єктів без SQL-посилань із `upstream-cache/`,
  старших за `UPSTREAM_ARTIFACT_ORPHAN_TTL`
- Мітки: немає
- Примітка: успішні видалення враховуються, навіть якщо наступний об'єкт завершує
  цикл cleanup із помилкою.

### `puppet_forge_upstream_artifact_cleanup_scanned_total`

- Тип: counter
- Значення: кількість об'єктів із `upstream-cache/`, переглянутих циклами cleanup
- Мітки: немає
- Примітка: враховує також свіжі об'єкти та об'єкти із SQL-посиланнями, які не видалялись.

### `puppet_forge_upstream_artifact_cleanup_failures_total`

- Тип: counter
- Значення: кількість окремих об'єктів із `upstream-cache/`, які worker не зміг видалити
- Мітки: немає
- Примітка: worker продовжує обробляти наступні об'єкти, а цикл також збільшує
  `puppet_forge_upstream_artifact_cleanup_total{result="error"}`.

### `puppet_forge_upstream_artifact_cleanup_duration_seconds`

- Тип: histogram
- Значення: тривалість обраних через lease циклів очищення кешу upstream-артефактів
- Мітки: немає
- Примітка: враховує успішні цикли та цикли з помилками.

## Примітки щодо кардинальності

Більшість метрик інвентарю агрегуються за обмеженими enum або власником модуля.
Метрики споживачів релізів є навмисним обмеженим винятком: вони використовують
операторські назви токенів і версії модулів, обмежують cached export через
`METRICS_RELEASE_CONSUMER_LIMIT`, явно показують truncation і зберігають SQL rows
лише протягом `RELEASE_CONSUMER_TTL`. Raw credentials, token IDs/digests/prefixes,
користувачі, request ID і тексти помилок ніколи не стають мітками метрик.
