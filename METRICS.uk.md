# Метрики

[English version](METRICS.md)

> **Runtime-контекст:** [SCHEMA.uk.md](SCHEMA.uk.md) пояснює, які метрики є
> локальними для репліки, які gauge описують спільний стан і як правильно
> агрегувати їх у multi-instance розгортанні.

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

### `puppet_forge_artifact_deletion_total`

- Тип: counter
- Значення: спроби обробки надійного видалення локальних артефактів
- Мітки:
  - `result`: `deleted`, `canceled` або `error`

`canceled` означає, що до cleanup той самий content-addressed path знову став
частиною опублікованого релізу. Worker прибрав застаріле завдання outbox, не
видаляючи чинний об'єкт.
`error` залишає завдання в черзі та відкладає наступну спробу на одну хвилину,
щоб один проблемний об'єкт не блокував наступні записи.

### `puppet_forge_artifact_deletions_pending`

- Тип: gauge
- Значення: кількість локальних артефактів, які очікують видалення у спільному SQL outbox
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

### `puppet_forge_upstream_refresh_last_duration_seconds`

- Тип: gauge
- Значення: тривалість останнього циклу upstream refresh у секундах

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

Метрики інвентарю агрегуються за обмеженими enum або власником модуля. Назви
окремих модулів, версії релізів, користувачі, токени, request ID і тексти помилок
ніколи не використовуються як мітки метрик.
