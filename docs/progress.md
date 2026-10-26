# Ход реализации Relay

Работа выполнена по `PLAN_MULTIPROTOCOL_CLIENT.md`: этапы 0–8 реализованы,
локальная unsigned поставка 0.1.0 подготовлена без публикации.
После установки пользователем GTK3/WebKitGTK подтверждено нативное Linux-окно;
исполнение shell на Windows/macOS остаётся отдельной проверкой платформ.

| Этап | Содержание | Состояние |
| --- | --- | --- |
| 0 | Проверка протоколов, динамического Protobuf и Wails v3 | Проверен, включая native Linux smoke |
| 1 | Desktop UI, SQLite, workspace, секреты | Реализован; restart E2E и native Linux проходят |
| 2 | Полный HTTP-движок и REST UI | Реализован; итоговая проверка ниже |
| 3 | Полный импорт Protobuf и редактор | Реализован; offline import/editor и restart E2E проходят |
| 4 | gRPC unary и reflection | Реализован; Go race/vet, 77 UI tests и четыре E2E проходят |
| 5 | Все gRPC streams и UI | Реализован; интерактивный bidi E2E и нативный server stream проходят |
| 6 | WebSocket | Реализован; Go race/vet, 123 UI tests, шесть E2E и native text echo проходят |
| 7 | Удобство и совместимость | Реализован; 139 UI tests, восемь E2E и перенос трёх протоколов в чистый профиль проходят |
| 8 | Безопасность, производительность, дистрибуция | Локальный Gate пройден; security/audit/lifecycle, 139 UI tests, девять E2E, native production smoke и повторяемость Linux/Windows packages подтверждены |

## Исходное состояние

В репозитории были README, Go module и пример healthcheck из Onyx. Пример
импортирует отсутствующие generated-типы другого проекта; он сохранён без
изменения содержимого как `examples/onyx/healthcheck.go.txt` и не входит в сборку.
Пример демонстрирует нестандартный gRPC framing поверх HTTP/3. В Relay стандартный
gRPC выполняется через grpc-go, а HTTP/3 остаётся отдельным HTTP-транспортом.

## Среда

- Go 1.26.10, Linux amd64; версии в результатах предыдущих этапов сохранены как исторические.
- GTK 3.24.52, WebKitGTK 2.52.6 установлены пользователем. Проверены сборка
  `-tags gtk3` и настоящее окно Relay, загрузка React assets и Go bridge.
- Проверки протоколов выполняются на локальных тестовых серверах, без внешних API.

## Результат этапа 0 на 2026-10-23

Реализованы настоящий HTTP/1.0 по TCP/TLS и forced HTTP/3 через QUIC с безопасным
TLS по умолчанию, custom CA, deadlines/cancel и лимитами тела/заголовков. Вложенные
`.proto` компилируются локально без `protoc`, реестр разрешает WKT/Any. Strict
ProtoJSON и ограниченный JSON5-like создают реальные `dynamicpb.Message`, сохраняя
точные int64; ошибки содержат позиции без копирования чувствительных значений.

gRPC-core выполняет unary и все streaming flags через grpc-go, поддерживает
metadata/status/trailers, независимые Send/Recv, half-close, cancel, TLS/mTLS и
лимиты. `cmd/spike` связывает импорт, редакторский ввод и настоящий gRPC transport;
сквозные unary/bidi тесты проверяют содержимое binary wire, а не JSON-эмуляцию.

Wails v3 beta.34 изолирован в `spikes/desktop`. Реальный Go→UI bridge в server
mode передал 10 000 событий с сохранением порядка; Playwright проверил отмену
долгой задачи и повторный запуск. На каждую порцию требуется ACK из UI; в полёте
максимум одна порция, входная очередь ограничена 128 событиями, порция — 64.

### Проверки

- `go test -race ./...` и `go vet ./...`: Go core, локальные HTTP/1.0/H3/gRPC,
  ProtoJSON, диагностика импорта/валидации, TLS errors, cancel/deadline, malformed
  input, limits, stream errors, сквозные CLI unary/bidi.
- `CGO_ENABLED=0 go test -tags server ./...` и `go vet -tags server ./...`
  в `spikes/desktop`: сборка shell API и проверки контроллера.
- `go test -race service.go service_test.go`: контроллер очереди, ACK,
  cancellation, restart и shutdown независимо от native Wails/GTK.
- `npm test` в `spikes/desktop`: 1 Playwright E2E, успешно (10 000 events,
  cancel, restart, без browser errors).
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build` в desktop spike: успешно.
- Подготовлен CI: core tests на трёх ОС, Linux race и browser smoke, native
  desktop build matrix. Удалённые CI jobs в этой сессии не запускались.

### Что осталось и риски

На момент этапа 0 нативный запуск блокировали системные библиотеки; после их
установки Linux smoke пройден. Windows/macOS runtime и реальные keychain остаются
непроверенными; Wails beta и platform integrations остаются риском. Полный HTTP
matrix, streaming files, redirect/proxy и редактор добавлены в этапе 2 ниже.
Reflection и WebSocket ещё предстоят. Подробности: `protocols.md` и ADR 001.

### Изменённые файлы

- `go.mod`, `go.sum`: закреплённый стек Go-core, удалены зависимости старого CLI.
- `internal/protobuf/{importer,registry,jsoncodec}` и `testdata/proto`: импорт,
  resolver, сериализация, fixtures и тесты.
- `internal/transport/httpclient/{http10,http3}` и общий `client.go`: реальные
  HTTP transports и тесты. `internal/transport/grpcclient`: RPC/streams и тесты.
- `internal/session`: bounded batches и тесты; `cmd/spike`: CLI и интеграция.
- `spikes/desktop`: отдельный Wails module, assets, controller, Go/Playwright
  tests, закреплённые npm dependencies и lockfile.
- `.github/workflows/checks.yml`, `.gitignore`: проверки и игнорирование артефактов.
- `README.md`, `docs/*`, `PLAN_MULTIPROTOCOL_CLIENT.md`: команды, архитектура,
  ограничения и отмеченные проверенные пункты.
- `healthcheck.go` перемещён без изменения содержимого в
  `examples/onyx/healthcheck.go.txt` как внешний reference с отсутствующими Onyx types.

## Результат этапа 1 на 2026-10-23

Реализован основной desktop shell `cmd/desktop` с production assets React 19,
TypeScript, Vite, Zustand и CodeMirror. Типизированные Wails bindings генерируются
из Go `internal/app.Service`. Слева доступны workspace, коллекции и вложенные
папки с CRUD, справа — вкладки, request editor, headers/body и нижняя панель
результата. Выполнение из UI явно помечено «планируется»; HTTP/gRPC-core этапа 0
не подменяется браузерной сетью.

SQLite хранит workspace, коллекции, папки, определения запросов, окружения,
настройки и ссылки открытых вкладок. Транзакционные миграции v1/v2 имеют checksum
и проверку версии, изменения snapshot используют revision/CAS, FK включены,
работает WAL. Профиль блокируется до открытия БД, чтобы два процесса не
обрабатывали одновременно SQLite/keychain.

Есть autosave с сериализацией и защитой новых edits при in-flight save, dirty
indicator и подтверждение закрытия вкладки. Transient body остаётся только в
памяти, явное сохранение включает body opt-in. UI-state содержит только вкладки
активного workspace. Реализованы dark/light/system theme, preview/history limits,
окружения и активное окружение workspace.

Секреты сохраняются через OS keyring adapter, в SQLite — только случайный ref.
Нет plaintext fallback; недоступность провайдера не блокирует обычный workspace.
Проверка keychain не держит app mutex. Значения не возвращаются в `LoadState`;
форма показывает password input. Удаление/замена используют durable outbox, чтобы
отказ SQLite или заблокированный vault не повреждал активные секреты.
Переменные имеют безопасную текстовую/JSON подстановку с redacted preview.

### Пройденные проверки

- `go test -race ./...` и `go vet ./...`: старое протокольное ядро, storage,
  app, secrets, profile lock. Дополнительные app regressions после review:
  keychain probe не блокирует LoadState; explicit clear окружения удаляет старый
  vault item только после snapshot commit.
- Storage: restart, CRUD, FK/cascade/циклы, concurrent CAS, rollback,
  последовательность/checksum миграций, upgrade v1→v2, durable outbox,
  отсутствие plaintext и body без opt-in в SQLite/WAL.
- App/security: сохранение и повторное открытие с неизменным secret ref,
  ошибочный Save/Set/Delete, locked vault + restart cleanup, маскирование,
  безопасные ошибки/JSON escaping, exact int64, limits, context.
- `npm run build`, `npm run lint`, `npm run format:check`: успешно.
- `npm test`: 13 тестов (7 Zustand store + 6 component tests), включая
  autosave races, dirty close, nested folder, секреты и workspace isolation.
- `npm run test:e2e`: настоящий Wails bridge + временная SQLite. Созданы новый
  workspace, коллекция, папка, запрос и окружение; изменены body/settings/theme;
  после остановки и запуска нового процесса всё восстановлено. Проверены
  transient body, close cancel/discard, отсутствие canary в SQLite/WAL/логах и
  каскадное удаление папки с запросом. Browser errors отсутствуют.
- `CGO_ENABLED=0 go test -tags server ./...` и `go vet -tags server ./...` в
  `cmd/desktop`: успешно; Windows amd64 cross-build основного приложения проходит.
- CI расширен frontend checks/E2E и сборкой основного shell на Linux/Windows/macOS.
  Удалённый CI в этой сессии не запускался.

### Изменённые файлы этапа 1

- `internal/domain/models.go`: модели workspace/request/environment/settings/UI.
- `internal/storage/*`: SQLite, SQL migrations и интеграционные тесты.
- `internal/security/*`: keyring, resolver и тесты без изменения реальных keychain.
- `internal/app/*`: application commands и тесты согласованности БД/vault.
- `internal/desktop/instancelock/*`: платформенная блокировка профиля и тест.
- `cmd/desktop/*`: основной Wails entrypoint и закреплённые module dependencies.
- `frontend/*`: UI, generated bindings, assets embed, npm/Go modules, lockfile,
  component/store/E2E tests и build/lint/format конфигурация.
- `tools/bindings/main.go`: воспроизводимая генерация TypeScript bindings.
- `go.mod`, `go.sum`, `.gitignore`, `.github/workflows/checks.yml`, README, план,
  архитектура/матрица протоколов и ADR 002.

### Оставшиеся ограничения

Linux webview теперь запущен; Windows/macOS runtime ещё не проверен.
Реальная запись в keychain каждой ОС не тестировалась:
адаптеры подключены, безопасность и сбои проверены с внедряемым vault. Upstream
keyring API не прерывает уже открытый OS prompt; queued cleanup повторится после
его завершения/разблокировки. Выполнение HTTP через Send добавлено на этапе 2.

## Результат этапа 2 на 2026-10-23

HTTP Send/Cancel основного UI выполняет сеть через Go. Engine поддерживает Auto,
Force HTTP/1.0, HTTP/1.1, HTTP/2, h2c prior knowledge и HTTP/3. Forced modes
не делают silent fallback. HTTP/1.0 пишет настоящий request-line; известный
размер файлов/multipart вычисляется до потоковой передачи, включая пустые файлы.
Standard HTTP/2 требует TLS ALPN h2; HTTP/3 использует QUIC/UDP.

Редактор получил query/path parameters, повторные headers с подсказками, raw,
JSON/JSON5-like, form-urlencoded, multipart и binary files. Доступны Basic,
Bearer, API key header/query, наследование auth от папки/коллекции, session cookie
jar, redirects, HTTP/HTTPS/SOCKS5 proxy, deadlines, custom CA, SNI, mTLS и explicit
insecure. TLS private keys и пути файлов не переходят через webview: native file
picker выдаёт временные opaque grants. После рестарта файлы выбираются заново.

Ответ показывает числовой status, фактический протокол, headers/trailers, TLS,
redirect chain и доступные timings. Pretty/Raw/Hex не исполняют HTML и сохраняют
точные числовые токены. Форматирование ограничено размером, глубиной и выходным
бюджетом; hex — 64 KiB. Preview по умолчанию 5 MiB, максимум 32 MiB. Полный ответ
пишется в файл 0600 до 256 MiB по умолчанию, пользовательский максимум — 1 GiB.
До восьми запросов выполняются одновременно, хранятся до 16 результатов с общим
бюджетом retained previews 64 MiB. Cancel, закрытие/удаление вкладки, eviction и
shutdown освобождают ресурсы; ошибочная или отменённая загрузка явно частичная.

Известные секреты маскируются в результате и безопасных diagnostics; полный
download по явному native Save сохраняет полученные байты, о различии есть
предупреждение. Cross-origin redirects удаляют credentials, чувствительные query
keys и Referer; mTLS-профиль блокирует переход на другой origin. Декомпрессия
ограничена восемью слоями и лимитом распакованного тела. SQLite не сохраняет
response history, file grants и прямые auth credentials. Transient body/auth/files
вызывают предупреждение при закрытии даже после autosave определения.

### Пройденные проверки

- `go test -race ./...` и `go vet ./...` всего Go-core. После последних мелких
  diagnostics/empty-file изменений дополнительно прошли app race/vet.
- HTTP engine: GET/POST во всех шести режимах, raw TCP request-line 1.0,
  несовместимые Force modes без fallback, streaming uploads/downloads, cancel,
  deadlines, header timeout, TLS trust/insecure/mTLS/isolation, HTTP/HTTPS/SOCKS5
  proxy, NO_PROXY, cookies, redirects и stripping credentials, gzip/deflate.
- App/storage: detached bridge context, JSON5/int64/variables, auth inheritance,
  streaming file/multipart >5 MiB и пустой HTTP/1.0 file, TLS profiles, partial
  downloads, budgets/eviction, shutdown, download mode 0600, отсутствие transient
  body/grants/literal auth в SQLite/WAL, immutable draft.
- `npm test`: 42 unit/component/store tests. Проверены cancellation polling,
  asynchronous cleanup, close/delete/late Start, obsolete autosave, exact int64,
  formatter resource limits и transient credentials warnings.
- `npm run lint`, `npm run format:check`, TypeScript и production Vite build.
- `npm run test:e2e`: два настоящих Wails bridge E2E. Первый проверяет SQLite
  restart и CRUD/settings/environments/tabs; второй отправляет JSON5/int64/auth,
  повторные query keys, проверяет trailers/redirects/gzip, неисполнение HTML,
  truncation и реальный disconnect при Cancel. Нет browser errors, запросов
  браузера к целевому API и token canary в SQLite/WAL/логах.
- `CGO_ENABLED=0 go test -tags server ./...` и `go vet -tags server ./...` в
  `cmd/desktop`; native GTK3 build и Windows amd64 cross-build.
- Настоящее Linux-окно Wails с GTK 3.24.52 / WebKitGTK 2.52.6 отрисовало рабочий
  интерфейс этапа 2 и загрузило workspace через bridge. Сетевые UI-действия
  автоматизированно проверены в Chromium server mode; синтетический ввод XTest
  не подтвердил их в native WebKit, поэтому такая проверка отдельно не заявлена.

### Изменённые файлы этапа 2

- `internal/transport/httpclient/engine/*`, `http10/transport.go`: streaming
  transports, policies и собственные HTTP/TLS/QUIC/proxy fixtures.
- `internal/domain/{models,http}.go`: определения HTTP/auth/TLS и execution DTO.
- `internal/app/{service,http,http_prepare,files}.go`, `http_test.go`: запуск,
  отмена, variable/auth resolution, file grants, previews/spools и integration tests.
- `internal/storage/{http_validation,http_validation_test,validation}.go`:
  сохранение безопасных определений и очистка transient data.
- `cmd/desktop/{main,files,files_native,files_server}.go` и module graph:
  native dialogs, привязки сервисов и shutdown.
- `frontend/src/components/{RequestEditor,HTTPFields,HTTPResponse,EntityDialog}.tsx`,
  store/types/bridge/response helpers, generated bindings, стили и тесты;
  `frontend/e2e/app.spec.ts`: сквозной HTTP fixture.
- README, план, progress, architecture, protocols и ADR 001.

### Ограничения и следующий этап

Native runtime Windows/macOS и реальные системные keychain/file dialogs остаются
платформенными проверками; удалённый CI локально не запускался. Wails v3 — beta.
HTTP/1.0, h2c и HTTP/3 не поддерживают proxy; ручной CONNECT не поддержан.
Суммарный диск ограничен количеством записей и индивидуальными лимитами, отдельной
общей disk quota нет. Неизвестные приложению секреты в ответе автоматически
распознать нельзя. Запуск/отрисовка native WebKit подтверждены, автоматизация
HTTP-взаимодействия использует настоящий Go bridge в server mode.

Этап 3 — полный импорт Protobuf и редактор сообщений. Основной UI gRPC/WebSocket
пока явно помечает выполнение как планируемое.

## Результат этапа 3 на 2026-10-24

Раздел Protobuf импортирует один/несколько `.proto`, папку с относительными
путями, дополнительные native include directories и бинарные FileDescriptorSet
(`.protoset`, `.pb`, `.desc`). Компиляция работает офлайн через закреплённый
`protocompile`, без внешнего `protoc` и generated client. Пропущенные стандартные
WKT разрешаются автоматически; отсутствующие нестандартные imports, конфликты
файлов/полных имён, cycles и неподдерживаемые editions явно отклоняются.
Доступ за пределы include roots через traversal/symlinks запрещён.

Migration v3 добавляет `proto_schemas`. В workspace сохраняется самодостаточный
descriptor set с зависимостями/source comments, SHA-256 и metadata. Артефакт
хранится отдельной приватной колонкой и исключён из обычного JSON bridge;
в UI возвращаются metadata и запрошенная инспекция. Исходники не нужны после
рестарта. Переимпорт сохраняет schema ID, атомарно заменяет artifact и сообщает
об изменениях выбранных методов, streaming flags и используемых message graphs,
включая presence/enum semantics. Удаление схемы очищает только её ссылку в
сохранённых запросах и открытых drafts, сохраняя method/body; queued autosave
повторно проверяет актуальность ссылки.

Инспектор показывает package → service → method, input/output, RPC kind,
fields, enum values, maps, oneof, required/presence и comments. Из метода можно
создать gRPC-запрос. Его редактор поддерживает Strict ProtoJSON/JSON5-like,
Validate, Format, Generate Example, canonical JSON и bounded hex preview.
Проверка создаёт `dynamicpb.Message`, кодирует deterministic binary Protobuf,
декодирует wire обратно и получает canonical ProtoJSON. Int64/uint64 не проходят
через float64 и не округляются. Для неизвестных полей, несовместимых типов,
oneof/maps/repeated/WKT показаны line/column и безопасный schema field path;
чувствительные значения/неизвестные keys/type URLs не попадают в errors.

Проверка является локальной операцией: environment/keyring secrets не читаются,
placeholders в строковых полях остаются буквальными. Body сохраняется только
после opt-in; previews/wire не сохраняются в SQLite. Вызов RPC пока явно
помечен «планируется». Runtime method path всегда получается из дескриптора.

Лимиты: 256 roots / 512 dependency files, 2 MiB на `.proto`, 16 MiB суммарных
sources/descriptor set, 8192 scanned entries; до 64 схем в общем 32 MiB snapshot
с учётом base64 и остальных данных. Input/wire — 4 MiB, depth 100; canonical
JSON — до 8 MiB с compact fallback, template — 256 KiB / depth 12 / 2048 fields.
До двух операций импорта/инспекции/валидации выполняются одновременно, без
накопления очереди больших inputs; busy/timeout ошибки допускают повторное чтение
без переимпорта. Deadline 30 секунд и application shutdown отменяют work.
UI использует поиск, ограниченные списки и страницы полей для больших схем.

### Пройденные проверки

- Общие `go test -race ./...` и `go vet ./...`: протокольное ядро предыдущих
  этапов, schema import/registry/codecs, app, SQLite, secrets и profile lock.
- Importer/registry: nested/source/folder/include imports, offline WKT,
  descriptor roundtrip с comments, missing deps, duplicate/FQN conflicts,
  malformed descriptor sets, cycles, traversal/symlinks, limits, cancel,
  Edition 2023 и явный отказ для Edition 2024.
- ProtoJSON: все scalar types, exact signed/unsigned int64, enum names/numbers,
  bytes/base64, nested/repeated/maps/oneof/optional/presence, WKT/Any,
  proto2 required/default/extension cases, safe diagnostic paths и positions,
  deterministic wire roundtrip, malformed binary и bounded pretty/templates.
- App: restart/private artifact, no environment/keyring reads, no validation
  writes, workspace isolation, native/browser descriptor import, reimport
  changes транзитивных messages и syntax/presence, delete без потери запросов,
  fail-fast budget и shutdown cancellation.
- Storage: v3 migration/checksum/DDL rollback, restart/private artifact,
  неизменность draft, checksum/encoding/path/ownership validation, сохранение
  artifact при обычных mutations и удаление вместе с workspace.
- Frontend: 53 unit/component/store tests, ESLint, Prettier, TypeScript и Vite
  production build. Новые regressions проверяют imports/includes/descriptor
  bytes, дерево/fields/create request, canonical int64/hex, stale validation,
  Format/Example, delete + queued autosave и Retry после busy/error.
- Playwright: три настоящих Wails bridge E2E. Предыдущие restart/CRUD и HTTP
  сценарии сохранены; новый импортирует папку с nested imports/WKT, создаёт
  запрос, проверяет exact int64/uint64/Any и binary hex, безопасный path error,
  restart/schema restoration, streaming reimport notice и binary descriptor
  import. Browser errors отсутствуют, diagnostic canary не попадает в логи.
- `CGO_ENABLED=0 go test -tags server ./...` / `go vet -tags server ./...` в
  `cmd/desktop`, GTK3 native build и Windows amd64 cross-build.
- Настоящее GTK3/WebKitGTK-окно этапа 3 загрузило сохранённую схему и gRPC draft,
  получило через Go bridge типы input/output и список методов. Полный
  import/Validate/Format interaction проверен автоматизированно в server mode.

### Изменённые файлы этапа 3

- `internal/protobuf/{importer,registry,jsoncodec}/*`, `testdata/proto/matrix/*`,
  `testdata/descriptors/*`: bounded compiler/descriptor import, inspection,
  codecs, safe diagnostics/templates и fixtures/tests.
- `internal/domain/{models,protobuf}.go`: schema metadata/private artifact,
  schema/mode references в gRPC definitions.
- `internal/storage/{storage,validation,protobuf,protobuf_test,storage_test}.go`,
  `migrations/003_proto_schemas.sql`: persistence, validation, migration/tests.
- `internal/app/schema*.go`, `service.go`: bound schema commands, native helper,
  compatibility notices, work budget, workspace cleanup и integration tests.
- `cmd/desktop/{files,protobuf}.go`, `go.mod` / `go.sum`: native file/include
  selection с opaque grants и binary descriptor import.
- `frontend/src/components/{SchemaManager,ProtoImportDialog,ProtoSchemaDetails,
  ProtoMessageEditor,ProtoDiagnostics,RequestEditor,Sidebar}.tsx`, App,
  types/store/bridge, generated bindings, styles и unit/component/E2E tests.
- README, план, progress, architecture и protocol matrix.

### Оставшиеся ограничения

ZIP, remote imports, reflection и RPC execution из UI пока не реализованы.
Поддержка syntax подтверждена для proto3, протестированной proto2-матрицы и
Edition 2023; новые editions не заявлены. Переимпорт выдаёт консервативные review
notices и не заменяет полный wire compatibility analyser. Для переимпорта
исходники/include roots выбираются заново, произвольные пути из JS не принимаются.
Native Windows/macOS runtime, OS keychain/file dialogs и удалённый CI остаются
платформенными проверками, Wails v3 остаётся beta.

Следующим после этапа 3 был gRPC unary и server reflection; результат приведён ниже.

## Результат этапа 4 на 2026-10-25

gRPC unary подключён к основному UI через Go и generated Wails bindings.
Endpoint, TLS/plaintext, custom CA/mTLS, SNI/authority, повторные metadata,
binary `-bin` values в base64, request gzip, send/receive limits и deadlines
настраиваются в gRPC-вкладке. Native file grants переиспользуются для PEM;
пути и private key bytes не передаются webview и не сохраняются в запросе.
Plaintext сохраняет TLS profile неактивным и показывает предупреждение.

Send принимает snapshot текущего draft, сразу возвращает execution ID и готовит
запрос в worker вне storage mutex. JSON5-like нормализуется до подстановки
переменных активного окружения; string-only substitution сохраняет JSON escaping
и точные int64. Dynamic input/output и полный method path получаются из
MethodDescriptor; вызов выполняется grpc-go Invoke по HTTP/2 без generated stubs.
Offline Validate по-прежнему не читает окружение/keyring и не отправляет RPC.
Streaming methods явно требуют следующего этапа.

Результат содержит ProtoJSON/hex, размер, timestamps/connect/total timing,
initial headers, trailers, status code/message/details и фактическую TLS state.
Raw binary — настоящий полученный Protobuf payload после gRPC decompression,
скопированный из входного buffer codec, без gRPC framing и повторной сериализации:
порядок полей и duplicate singular tags сохраняются. Стандартные google.rpc
status details декодируются встроенными типами; custom types — локальным registry;
неизвестный Any получает безопасный binary fallback и пояснение.

Discover via Reflection вызывается явно: v1, v1alpha только при Unimplemented,
тот же TLS/authority/metadata profile. Полная dependency closure сохраняется
в SQLite как descriptor schema с checksum и доступна после restart офлайн.
Unary использует выбранную схему и не делает скрытый discovery. Отключённый
reflection даёт понятную подсказку о `.proto`/descriptor set. Session cache
fingerprints включает endpoint, authority, TLS profile, **разрешённый через
окружение SNI** и metadata; повторный discovery прежнего профиля обновляет тот же
schema ID. Fingerprints/credentials не сохраняются. Изменение resolved SNI
разделяет cache entries, даже если endpoint и текст шаблона остались прежними.

Start/Cancel/Forget, polling с generation checks и shutdown освобождают
соединения, не связывая lifetime RPC с завершившимся bridge-вызовом.
Закрытие/удаление вкладки отменяет её операцию; поздний ответ не попадает
в другой draft. Известные secrets маскируются в JSON, metadata, status, TLS
и errors. Binary previews с чувствительными данными скрываются; executions
не записываются в SQLite/logs. Sensitive literal metadata остаются в памяти;
сохранённые значения используют environment references.

Общий HTTP/gRPC redactor выполняет один проход с deduplication и приоритетом
длинных совпадений: вставленные `[REDACTED]` не обрабатываются повторно.
Bounded writer останавливает расширение до выделения чрезмерного output buffer,
включая короткие секреты в многомегабайтном ответе. JSON после redaction либо
остаётся валидным и ограниченным, либо опускается с предупреждением.

До proto.Unmarshal добавлен структурный guard descriptor wire: 100 000 work
units на узлы/поля/packed structures, depth 100. Общий бюджет применяется
к complete descriptor set и всем reflection responses, включая повторные files;
маленький по байтам allocation bomb отклоняется до создания descriptor graph.
Проверка дополняет byte/file limits и применяется также к локальным artifacts.

Defaults: общий deadline 30 секунд, connect 10 секунд, send/receive по 4 MiB
(настраиваются до 4 MiB), metadata 64 KiB. Go удерживает до четырёх активных
операций, 16 results и 64 MiB retained result budget с предварительным reserve;
UI — до 16 завершённых results и 32 MiB строк, активные операции не вытесняются.
ProtoJSON — 8 MiB, details — 128 элементов / 1 MiB в сумме. Reflection:
512 files / 16 MiB, 2048 services/requests, 30 секунд; session cache — 256 entries.
Временные buffers схем/codec отдельно от retained result budget; persistent
schemas остаются в общем ограниченном SQLite snapshot.

### Пройденные проверки

- Полные `go test -race ./...` и `go vet ./...`: protocol core, importer guard,
  app/storage/security, profile lock и regressions предыдущих этапов.
- gRPC transport: dynamic unary, v1/v1alpha и disabled reflection, dependency
  closure/conflicts/limits, gzip, repeated/binary metadata, rich status/details,
  exact received payload, proto2 extensions, TLS/mTLS и cancellation/deadline.
- App: 11 gRPC integration tests, detached bridge context, keyring cancellation
  без storage mutex, concurrency/eviction/shutdown, неизвестный Any, secret bytes
  и numeric ProtoJSON, short-secret expansion, неактивный TLS profile и
  SNI-variable cache separation/reuse. Полный `app -race` и `app vet` проходят.
- Descriptor allocation guards проверены до unmarshal на повторных messages,
  packed fields и повторных reflection responses. Общий redactor проверен
  с overlapping/короткими secrets и многомегабайтным output budget.
- Frontend: 77 unit/component/store tests, ESLint, TypeScript и Vite production
  build проходят; gRPC settings, snapshot send/cancel, late results, close/delete,
  reflection selection, bounded results, metadata persistence и response views.
- Четыре Playwright E2E прошли на финальных production assets за 25.6 секунды:
  restart/CRUD, HTTP, offline Protobuf и новый gRPC. Последний вызывает реальный
  Go transport с локальной и reflected схемой, v1/v1alpha, TLS, metadata,
  status/details, Cancel и восстановлением после restart.
- `CGO_ENABLED=0 go test -tags server ./...` / `go vet -tags server ./...`
  в `cmd/desktop`, native GTK3 build и Windows amd64 cross-build проходят.
- Native GTK3/WebKitGTK smoke этапа 4 прошёл: реальное окно 1440×900 загрузило
  production gRPC UI, Call/Discover и connection tabs, восстановленную offline
  схему и выбранный unary method через Go inspection. CodeMirror сохранил точный
  int64 и кириллицу в JSON5. RPC interactions проверены четырьмя E2E в Chromium
  через Wails server bridge; native click automation не проводилась.

### Изменённые файлы этапа 4

- `internal/transport/grpcclient/{client,codec,reflection}.go` и tests:
  connection diagnostics, compression/resolver, raw payload capture и discovery.
- `internal/protobuf/importer/{budget,budget_test}.go` и import paths:
  pre-unmarshal structural limits для локальных/reflected artifacts.
- `internal/domain/{models,grpc}.go`, `internal/storage/grpc_validation*.go`:
  connection/execution models, bounded settings и metadata persistence.
- `internal/app/grpc*.go`, `redaction*.go`, `service.go`: worker lifecycle,
  TLS/environment preparation, safe results, session cache и integration tests.
- `cmd/desktop/main.go`, modules/bindings и `frontend/src`: connection controls,
  Send/Cancel/reflection, bounded response views/store и unit/E2E fixtures.
- README, план, progress, architecture и protocol matrix.

### Оставшиеся ограничения

Этап 5 не начат: server/client/bidi controls и timeline ещё отсутствуют в UI.
WebSocket, долговременная история, ZIP и remote imports остаются следующими
задачами. Reflection refresh cache связывает profiles только в пределах процесса;
после restart сохранённая схема доступна, а новый explicit discovery может создать
новый schema ID. Ответы не являются долговременной историей.

Реальный OS keyring использует синхронный upstream API: открытый системный prompt
не прерывается context. UI Start/Cancel остаётся отзывчивым, worker и shutdown
могут ждать возврата системного вызова. Тестовый vault не подтверждает работу
реальных keychain на всех ОС и не изменяет пользовательские системные секреты.
Native Windows/macOS runtime, системные file dialogs/keychain и удалённый CI
остаются платформенными проверками; Windows cross-build не заменяет runtime,
Wails v3 остаётся beta.

Следующий функциональный этап — все gRPC streams и UI (этап 5).

## Результат этапа 5 на 2026-10-25

Все четыре вида gRPC RPC доступны в основном UI. Server stream отправляет одно
тело и принимает 0/N ответов. Client stream и bidi открываются без отправки;
Send message передаёт текущий текст редактора, Half-close закрывает только
направление к серверу, Cancel отменяет оба направления. Интерактивный bidi
проверен: второе сообщение отправляется после первого ответа в тот же RPC.
Endpoint, schema, TLS/metadata и environment фиксируются на время stream;
новое тело отдельно проверяется и преобразуется в dynamic Protobuf при каждом Send.

Send и Recv работают независимо. Конкурирующие Send/Half-close получают busy
error; команды до readiness, после half-close и завершения отклоняются. Ошибка
локальной валидации не закрывает stream. После EOF/ошибки остаются headers,
полученные trailers/status/details и принятая история. При Cancel заполненной
очереди доступный final status собирается ограниченным дополнительным чтением;
при достижении предела действует context status и предупреждение о пропусках.

Go передаёт сообщения порциями через Poll/ACK: до 32 сообщений / примерно 1 MiB
за порцию, одно большее сообщение отдельно. Неподтверждённая очередь ограничена
64 сообщениями / 14 MiB; ещё 2 MiB резервируются на status/metadata внутри
16 MiB на stream и общего gRPC budget 64 MiB. При полной очереди producer ждёт
ACK или отмену. Повторный ACK безопасен, нельзя подтвердить недоставленное.
Terminal stream с pending messages не вытесняется новым запуском; очередь
можно дочитать или явно освободить. Временные codec/dynamic/schema buffers
и ограниченный keyring cache отдельно от retained result budget.

UI подтверждает сообщения после принятия в собственную ограниченную историю,
устраняет дубликаты при retry и дочитывает terminal batch до пустой порции.
История хранит направление, индекс, общий sequence, время, размер и JSON/hex;
для входящих сообщений сохранён исходный decompressed payload, для исходящих
показана сериализация отправленного message. Отрисовываются 50 строк и одно
выбранное сообщение: до 262 144 символов JSON или 64 KiB hex. Число событий
ограничено настройкой и жёстким пределом 2 000, на stream — 24 MiB строк
с допуском одного большого сообщения в пределах общего UI budget 32 MiB.
Общий бюджет включает завершённые unary/reflection results и активную историю;
освобождение старых превью видно пользователю, active execution сохраняется.

Поздние Start/Send/Poll не меняют закрытую или новую вкладку. Закрытие/удаление,
смена workspace, dispose и shutdown отменяют работу и освобождают execution.
Сообщения и история не записываются в SQLite. Новые keyring secrets маскируют
следующие и ещё ожидающие ACK previews, status/metadata/errors; binary preview
скрывается. Повторный Send без новых секретов не перемаскирует redaction markers.

### Проверки

- Полные `go test -race ./...` и `go vet ./...` прошли; после финальных изменений
  Cancel/redaction повторно прошли полный `internal/app -race` и app vet.
- Transport: 0/N server messages, client response cardinality 0/1/2, exact
  received wire, TLS peer, gzip, extensions/recursion, concurrent Send/Recv,
  blocked write/half-close, slow consumer и Cancel.
- 11 новых app integration tests: все stream-типы, интерактивный bidi,
  zero-message half-close, EOF/error/deadline, ACK/cursors/budgets, terminal drain,
  admission без потери pending messages, immutable environment, keyring/busy,
  late-secret masking и повторный short-secret Send.
- 97 frontend tests, TypeScript, ESLint, Prettier и production Vite build.
  Новые проверки покрывают polling/ACK retry/dedup/drain, late results,
  half-close/cleanup, timeline selection/window и смешанный бюджет памяти.
- Пять Playwright E2E прошли за 32.1 секунды: прежние restart/CRUD, HTTP,
  offline Protobuf, unary/reflection и новый полный streaming-сценарий.
  Последний проверяет same-RPC bidi exchange, client 0/N, server 0/N,
  mid-stream rich status, bounded history, реальную отмену и закрытие вкладки.
  Browser requests к целевому API и ошибки отсутствуют; token canary не попал
  в SQLite/WAL/логи.
  После финальных Cancel/redaction и UI изменений оба gRPC E2E повторно прошли
  на окончательном коде за 18.2 секунды.
- Wails server test/vet, native GTK3 build и Windows amd64 cross-build проходят.
  Настоящее GTK3/WebKitGTK окно выполнило server stream по нажатию Start stream:
  сервер подтвердил один RPC с одним input, UI показал 0 OK, sent 1 / received 3
  и четыре записи истории с кириллицей и JSON. Остальные интерактивные RPC-типы
  проверены через настоящий Wails bridge в Chromium.

### Изменённые файлы

- `internal/transport/grpcclient/stream*.go`: exact receive payload, peer/timing,
  cardinality и транспортные regressions.
- `internal/app/grpc*.go`, `internal/domain/grpc.go`: stream lifecycle,
  Poll/ACK/commands, bounded history, secret snapshots и integration tests.
- `frontend/src/{lib,components}/*`, bindings и новые stream unit/component tests:
  controls, contextual shortcuts, bounded timeline и безопасный cleanup.
- `internal/testutil/grpcfixture/main.go`, `testdata/proto/streaming/streams.proto`,
  `frontend/e2e/grpc.spec.ts`: реальный dynamic server и сквозной streaming тест.
- README, план, architecture, protocol matrix и этот журнал.

### Оставшиеся ограничения

Следующий после этапа 5 — WebSocket; его результат приведён ниже. История gRPC stream остаётся в памяти
и ограничена лимитами, долговременное хранение ещё не реализовано. Если при Cancel
bounded drain не достиг peer status, доступны context status и уже полученные
metadata; получение всех trailers при прерывании не гарантируется.

Windows/macOS runtime, реальные системные file dialogs/keychain и удалённый CI
остаются отдельными проверками. Upstream keyring prompt не прерывается context;
worker/shutdown могут ожидать возврата системного вызова. Wails v3 остаётся beta.

## Результат этапа 6 на 2026-10-25

WebSocket подключён к основному UI через Go и `coder/websocket` 1.8.14.
Доступны `ws/wss` с HTTP/1.1 Upgrade, query/headers/Origin/Cookie/subprotocols,
TLS/custom CA/mTLS/insecure и proxy none/environment/custom HTTP/HTTPS/SOCKS5.
Connect фиксирует настройки и окружение; Send message передаёт text или binary
hex/base64/file в том же соединении. Ping ограничен одним вызовом в секунду,
Disconnect выполняет close handshake с code/reason, Abort прерывает socket.
Штатное закрытие и сетевой разрыв различаются в результате и timeline.

Один reader и сериализованные записи обрабатывают сообщения независимо от UI.
Go-очередь ограничена 64 событиями / 8 MiB; при переполнении удаляются старые
события, сохраняется счётчик пропусков. Это позволяет продолжать обработку
ping/pong/close при медленном потребителе. Poll/ACK каждые 150 мс передаёт до 32
событий / примерно 1 MiB. Для явного сохранения Go держит отдельную историю:
до 2 000 событий / 6 MiB. С резервом на metadata соединение занимает не более
16 MiB retained buffers, общий WebSocket budget — 64 MiB / четыре активных
соединения. Terminal execution с неподтверждёнными событиями не вытесняется
новым Connect. Временные buffers транспорта учитываются отдельно.

Превью text/base64 ограничено 64 KiB исходного payload. UI отображает окно из
50 строк, учитывает historyLimit до 2 000 событий, 24 MiB строк на соединение
и общий бюджет 32 MiB, включая открытый saved preview; завершённых результатов
не более 16. Потери и освобождение старых превью видны пользователю. Известные
секреты маскируются, binary с ними скрывается; environment snapshot ограничен
1 MiB / 1 024 переменными, keyring cache — 256 KiB. Закрытие вкладки, удаление,
смена workspace и shutdown освобождают соединение и результаты.

«Сохранить сессию» доступно только после завершения. По умолчанию сохраняются
metadata и события без payload; opt-in добавляет только обработанные ограниченные
превью, без отдельного архива полных сообщений. URL query и чувствительные
headers исключаются всегда. Сессии можно просматривать и удалять после restart.
SQLite migration 4 добавляет отдельную историю, независимо от snapshot CAS:
обычный autosave её не стирает, удаление владельца очищает связанные сессии.
Лимиты — 8 MiB на запись, 16 сессий / 32 MiB всего с удалением старейших.
Перед typed JSON unmarshal проверяются 100 000 tokens, глубина 32, не более
2 000 событий и длина timestamps до 64 bytes, чтобы ограничить выделение памяти.

### Проверки

- Transport: 13 test functions, race и vet проходят, включая handshake,
  text/binary, ping/pong/close, TLS/mTLS, proxy, timeout и аварийное отключение.
- 123 frontend tests, TypeScript, ESLint, Prettier и production Vite build проходят.
- Storage race/vet проходят: migration v3→v4, payload opt-in, restart,
  snapshot autosave, ownership, безопасная очистка и структурные лимиты.
- 13 новых WebSocket app integration tests и все прежние HTTP/gRPC app tests
  проходят с race за 11.264 секунды; app vet проходит. В том числе проверены
  immutable environment, ACK/gap/budgets, privacy отдельных Cookie values,
  binary hex с пробелами, cancellation и сохранение сессии.
- Полные `go test -race ./...` и `go vet ./...` проходят, как и Wails server
  test/vet, native GTK3 build и Windows amd64 cross-build.
- Все шесть Playwright E2E прошли за 56.8 секунды. Новый WebSocket сценарий
  прошёл за 8.9 секунды: handshake/headers/subprotocol, text/binary, ping/pong,
  поток 10 000 событий, TLS error/insecure, HTTP 401, штатное закрытие,
  metadata-only/payload opt-in history и restart.
- Настоящее GTK3/WebKitGTK окно подключилось к fixture и выполнило text echo:
  сервер подтвердил одно соединение и одно сообщение, UI показал open / HTTP 101 /
  `echo.v1` и sent/received по 35 bytes — «Привет из GTK3 · WebSocket».
  Остальные взаимодействия проверены через настоящий Wails bridge в Chromium.
  Generated bindings содержат 53 метода и 50 моделей. Gate этапа 6 пройден.

### Изменённые файлы

- `internal/transport/websocketclient/*`, `internal/app/websocket*.go`,
  `internal/domain/websocket.go`: transport, lifecycle, Poll/ACK, privacy и tests.
- `internal/storage/websocket*.go` и migration 4: request options и saved sessions.
- `frontend/src/{components,lib}/*`, generated bindings, unit/component tests:
  controls, bounded timeline, session history и cleanup.
- `internal/testutil/wsfixture/main.go`, `frontend/e2e/websocket.spec.ts`:
  локальный реальный server и сквозной сценарий; README, план и protocol docs.

### Ограничения

Отображаются логические messages, отдельные wire frames и fragmentation не
редактируются. Heartbeat, автоматический reconnect/backoff и WebSocket поверх
HTTP/2/HTTP/3 не реализованы. Сохранённая история содержит ограниченные превью;
она не является архивом полных сообщений. Windows/macOS runtime, реальные
системные dialogs/keychain и удалённый CI требуют своих платформенных проверок.
Результат следующего функционального этапа — удобство и совместимость — приведён ниже.

## Результат этапа 7 на 2026-10-25

Добавлен перенос одного workspace в native JSON `relay-project/v1` с
HTTP/gRPC/WebSocket definitions, коллекциями, вложенными папками, окружениями
и самодостаточными Protobuf descriptor artifacts. Export выдаёт transfer-local
IDs, Apply повторно заменяет все IDs и ссылки, создавая новое workspace.
Существующие данные, настройки, открытые вкладки и drafts сохраняются.
Preview показывает название, количество сущностей и ограниченный отчёт без
записи в SQLite; применение повторно проверяет исходный файл. Definitions
и artifacts добавляются одним CAS commit с откатом всех записей при ошибке.

По умолчанию bodies, form и multipart values исключены. Bodies можно включить
явно при экспорте и при импорте. Secret values, keyring references, file grants,
TLS file references, локальные multipart filenames и response/session history
не переносятся. Secret variables остаются пустыми объявлениями; публичные
переменные с чувствительными именами и credential usages становятся секретными.
Literal credentials очищаются из auth, headers/query, Cookie и URL userinfo;
известные копии и URL/base64/hex представления удаляются из прочих текстовых
полей. Vault не читается и не изменяется. Descriptor с известным credential
или его закодированной копией отклоняется без переписывания бинарной схемы.
После импорта секреты вводятся повторно, локальные файлы выбираются заново.
Native dialogs читают bounded
UTF-8 JSON и сохраняют через private temporary sibling и rename; browser server
mode использует file input/download.

Postman Collection v2.1 и Insomnia JSON export v4 преобразуются локальными Go
parsers в тот же Document и проходят общий sanitizer, проверку ownership и
атомарное сохранение. Поддерживаются вложенные HTTP коллекции, повторные и
отключённые поля, basic/bearer/API-key auth, простые переменные и документированные
body modes. Unsupported scripts/tests/flows/settings/resources попадают в отчёт;
код не исполняется. Другие версии, ambiguous JSON, cycles/orphans и неверные
типы отклоняются. Version-specific fixtures добавлены в `testdata/project`.
Insomnia JSON с unquoted `{{count}}` сохраняется как исходный body с
`json-body-review`: Go resolver подставляет templates внутри JSON strings,
поэтому такие typed placeholders нужно проверить и поправить перед Send.
Полные правила: [native-project.md](native-project.md),
[import-compatibility.md](import-compatibility.md).

Общий лимит — 32 MiB JSON, depth 32, 200 000 tokens, 10 000 сущностей с
переменными, 2 000 запросов и до 200 warnings. Descriptor artifact ограничен
16 MiB и дополнительными Protobuf budgets. Санитизация допускает до 1 024
credential literals / 1 MiB; чрезмерный ввод отклоняет весь перенос. Тяжёлые
операции используют два offline work slots и timeout 30 секунд.

В списке коллекций добавлены поиск по имени запроса, URL/endpoint, HTTP/RPC
method и ancestor folders/collections; фильтр протокола и сортировка по имени
сохраняют контекст дерева. «История запросов» показывает последний доступный
результат каждого запроса в текущем процессе, фильтруется по имени/адресу/статусу,
протоколу и состоянию, сортируется по времени либо имени. Это ограниченный
текущий набор результатов, отдельная сохранённая WebSocket history остаётся
в панели ответа. Показаны первые 100 строк отфильтрованной recent history.

Ctrl/⌘ K фокусирует поиск, Ctrl/⌘ Tab и Shift переключают открытые вкладки,
Ctrl/⌘ , открывает настройки; прежние Send/Cancel/Save/Close shortcuts сохранены.
Ширину списка и высоту редактора можно менять pointer drag либо стрелками
на доступном separator; Shift меняет шаг, Home/End задают пределы, double-click
восстанавливает размер по умолчанию. Отчёты импорта и ошибки отображаются как
текст, поздние file/preview/export results игнорируются после смены режима
или закрытия диалога.

### Проверки

- Native: пять test functions для roundtrip трёх протоколов, fresh IDs,
  body opt-in, privacy, checksum/descriptor/ownership, encoded credential copies
  в descriptor comments и JSON limits проходят.
- App: два integration tests проверяют перенос с offline exact int64,
  restart, сохранение существующих данных и отсутствие vault calls; неуспешный
  preview/apply сохраняет предыдущую revision. Targeted race проходит.
- Storage: два tests проверяют graph validation без записи, полный rollback
  после принудительной ошибки уже после записи artifacts, CAS и outbox. Race
  проходит; vet project/app/storage проходит.
- External project package `-race` и vet проходят, включая версии, unsupported
  auth/scripts, disabled credentials, cross-field copies и bounded inputs.
- Два новых Playwright project E2E проходят: fresh-profile
  export UI/default bodies/body opt-in, импорт descriptors и настоящие
  HTTP/gRPC/WebSocket calls после повторного ввода runtime credentials;
  Postman/Insomnia preview/apply/version rejection. Canary отсутствует
  в целевой SQLite/WAL и логах.
- Полные `go test -race ./...` и `go vet ./...` проходят,
  включая app за 13.163 секунды и storage за 8.584 секунды. Final project
  package race проходит, включая encoded credentials в descriptor comments.
  После уточнения диагностики typed JSON body повторно прошли race
  project/app/storage (project 1.879 секунды, app 12.313 секунды) и project vet.
- Generated bindings: 58 методов и 57 моделей; TypeScript/Vite production
  build, ESLint и Prettier проходят. Полный Vitest suite: 139/139 tests,
  12 files, 6.58 секунды. Новые store/component проверки покрывают queued
  autosave/import/export, drafts, поздние results/dispose, modal hotkeys,
  навигацию, фильтры и доступные разделители.
- Все восемь Playwright E2E прошли за 49.5 секунды; новый native transfer
  Gate прошёл за 4.6 секунды, внешние форматы — за 1.5 секунды. Предыдущие
  HTTP, Protobuf, gRPC, WebSocket и restart сценарии также проходят.
  После финального уточнения import diagnostics оба project E2E повторно
  прошли за 10.0 секунды; GTK3 и Windows binaries пересобраны успешно.
- Desktop `CGO_ENABLED=0` test/vet проходят, включая два file helper tests:
  bounded selection и atomic save/cancellation. Итоговые GTK3 native и
  Windows amd64 binaries собираются. Настоящее GTK3/WebKitGTK окно открыло
  export UI и OS Save dialog; сохранён `relay-project/v1` JSON размером
  1 604 bytes с правами `0600`, одним WebSocket definition, пустыми body и
  file grant. В окончательной GTK3-сборке проверен также OS Open dialog:
  экспортированный файл выбран через File Chooser, UI выполнил preview и import.
  После применения SQLite содержит два workspace и два запроса: исходные
  данные сохранены, импорт добавлен отдельно. Тестовый процесс и профиль очищены.

### Изменённые файлы

- `internal/project/*`, `internal/domain/project.go`, `internal/app/project*.go`:
  форматы, privacy, offline verification, preview и import/export API.
- `internal/storage/project*.go`: единая validation и проверки atomic rollback;
  migration не требуется, descriptors используют существующую schema table.
- `cmd/desktop/project*.go`, generated bindings: native selection/save и bridge.
- `frontend/src/components/{ProjectTransferDialog,RecentExecutions,PanelSeparator,Sidebar}.tsx`,
  store/bridge/types и UI tests: transfer flow, поиск, история, shortcuts и панели.
- `frontend/e2e/project.spec.ts`, `testdata/project/*`, новые format docs,
  README, план, architecture/protocol matrix и этот журнал.

### Gate и оставшиеся ограничения

Gate этапа 7 пройден: экспортированный проект импортирован в чистый профиль,
descriptor schema работает офлайн, сохранённые HTTP/gRPC/WebSocket definitions
выполнили настоящие запросы после повторного ввода credentials. OpenAPI 3.0/3.1
и OAuth2 helpers не реализованы: это опциональные пункты плана, без заявления
совместимости. Следующий этап — безопасность, производительность и дистрибуция
(этап 8).

Windows/macOS runtime, системные file dialogs/keychain на этих платформах,
remote CI, подписывание и релизные пакеты остаются отдельными проверками.

## Результат этапа 8 на 2026-10-25

Security review исправил четыре конкретные проблемы. JSON template до typed
decode получил structural budget 100 000 при прежних 4 MiB/depth 64: короткий
массив больше не создаёт миллионы `any` одновременно в source/wire/redacted
деревьях. `.proto` source до parser/AST ограничен общим budget 100 000 lexical
tokens/comments и 100 000 переводов строки на схему; byte limits и directory
containment сохранены. Compiler diagnostics больше не копируют string literals,
custom enum option values и произвольные symbols. Фиксированные категории
сохраняют File/Line/Column, missing import — полезное virtual filename.

Postman/Insomnia sanitizer теперь удаляет raw, URL/base64/base64url/hex copies
известных credentials до потери исходного literal при замене на secret placeholder.
Negative ZIP test проверяет, что все три JSON parsers отклоняют архив с `../../`
entry без интерпретации имён и содержимого. Распаковка архивов не добавлялась.
Offline schema/project commands не читают vault и не активируют сеть.
[Threat model](security.md) описывает assets, trust boundaries и residual risks,
включая unknown secrets в opt-in bodies, непроверенный локальный профиль,
непрерываемый системный keyring prompt и исходный private HTTP response spool.

Общий `app.Shutdown` параллельно останавливает HTTP, gRPC и WebSocket. HTTP/gRPC
отменяются сразу, WebSocket получает до секунды на 1001 Going Away, затем
закрывается физический socket. Новые HTTP executions и file grants отклоняются;
retained previews, secrets, history, reflection cache, cookie jar и spools
освобождаются. Исправлены поздний HTTP launch после shutdown и удержание данных
в WebSocket/cookies. Присутствуют проверки смешанных protocol workers,
неотвеченного UI ACK, повторной/конкурентной остановки и закрытия WS 1001.

Добавлены benchmarks и CPU/heap profiles для real preview/queue code при
10 000 events. Общая secret check использует `bytes.Contains` вместо копирования
целого payload в `string`; before/after сократил cumulative allocations примерно
на 625 MiB для 10 000 × 64 KiB, сохранив прежние правила маскирования. В контрольном
burst retained размер остался 13.87 MiB при 625 MiB обработанных payload;
это учётный retention, а не предел RSS. Команды, среда и измерения находятся
в [performance](performance.md).

Desktop assets получают CSP с `default-src 'none'`, same-origin scripts/connect
и ограниченными остальными sources. HTML peer content остаётся текстом.
Asset request logging выключен; production build отключает devtools и
development logs. Server mode допускает только loopback и проверяет Origin/Host;
реальная bridge E2E проверяет blocked inline script и 403 для чужого Origin/rebind.
Это локальный development bridge, не публичный сервис с аутентификацией.

Go toolchain обновлён до 1.26.10; обновлены `x/net` 0.60.0, `x/crypto` 0.57.0,
`x/text` 0.42.0, `x/sys` 0.48.0 и `x/sync` 0.23.0. Initial vulnerability scan
нашёл достижимые вызовы в stdlib и `x/net`. Финальный govulncheck v1.4.0 для
root, desktop server и native `production,gtk3` завершился с exit 0: ноль
reachable и ноль imported-package vulnerabilities. Один module-only advisory
GO-2026-5932 относится к SSH-коду зависимости, который Relay не импортирует
и не вызывает; при добавлении SSH требуется повторный review. Vite обновлён
до 7.3.7, Vitest — до 4.1.12; npm audit сообщает ноль vulnerabilities.
CI использует text-mode govulncheck с failure exit status
и `npm audit --audit-level=moderate`, дополнительно к test/race/vet/UI/E2E.

Build metadata доступны через новый `GetBuildInfo`, настройки и `--version`
до открытия профиля/GUI: version, commit, target, Go toolchain и unsigned status.
Generated bindings содержат 59 methods и 59 models. Release tooling использует
production tags, fixed source epoch/linker metadata, stable archive order и
file modes. Создаются Linux GTK3 `.tar.gz`/`.deb`, Windows portable `.zip`,
а на native macOS — архив с `.app`. Проверка manifest, ожидаемой версии,
SHA-256 и archive inventory не запускает содержимое. Manual CI workflow
сохраняет workflow artifacts, не создаёт release/tag и не публикует приложение.

Все пакеты этой поставки **UNSIGNED**. Сертификаты не предоставлены;
signing/notarization и поиск ключей не выполнялись. Автоматической загрузки
и исполнения обновлений нет. Checksum без доверенного источника manifest не
подтверждает автора. Документация: [releases](releases.md),
[release notes](release-notes.md), [troubleshooting](troubleshooting.md).

### Проверки

- Security regressions и полные race packages проходят: security 1.118 секунды,
  project 2.048, importer 1.803, app 15.942. `go vet` этих четырёх пакетов проходит.
  Реальные compiler errors edition/custom option/symbol содержат canary до
  boundary и не содержат его после conversion в diagnostic DTO; keyring calls
  и запись исходников в SQLite отсутствуют.
- Полные `go test -race ./...` и `go vet ./...` на Go 1.26.10 проходят;
  desktop server test/vet проходят. Release validator после исправления ZIP
  inventory повторно прошёл race и vet.
- UI: 139/139 Vitest tests, ESLint и Prettier проходят; production frontend
  build успешен, bindings — 59 methods/59 models. Все девять Playwright E2E
  прошли за 55 секунд, включая прежние HTTP/Protobuf/gRPC/WS/project/restart
  сценарии и новый CSP/Host/Origin сценарий через настоящий Wails bridge.
- Две Linux и две Windows package builds при одинаковых frozen frontend
  assets (`--skip-frontend`) дали побайтно одинаковые packages, manifest и
  SHA256SUMS. Отдельный fresh `npm run build` повторно создал все три frontend
  files с прежними SHA-256. Воспроизводимость ограничена той же source snapshot,
  зависимостями, toolchain и системными native libraries.
- Linux deb проверен и извлечён без установки. Из portable package извлечён
  production GTK3 binary: настоящее окно подключилось к локальному WS fixture,
  отправило текст и получило echo в том же соединении; fixture подтвердил
  одно соединение и одно echo message. Settings показали 0.1.0 и UNSIGNED.
- Локальные artifacts скопированы в `dist/release/0.1.0/linux-amd64` и
  `dist/release/0.1.0/windows-amd64`; обе копии прошли package verifier.
  Каталоги gitignored. В manifests честно указан dirty/untracked source state:
  сохранённый base commit сам по себе не восстанавливает текущую рабочую копию.
  SHA-256 и ссылки находятся в [releases](releases.md).
- Финальные `git diff --check` и gofmt проходят; fixture-only formatting
  после cleanup проверено повторным Prettier. HTTP/gRPC/WS shutdown проверен
  race integration tests с repeated/concurrent calls и cleanup ресурсов.

### Gate

Локальный Gate этапа 8 пройден: критические сценарии, воспроизводимость пакетов,
audit и production Linux smoke подтверждены. Нативные GTK3 Open/Save dialogs
также подтверждены этапом 7. Definition of Done первоначальной локальной
поставки отмечен в плане с явными платформенными оговорками.

Пакеты не устанавливались системным package manager и не публиковались.
Windows/macOS GUI, dialogs и реальные OS keychain на этих платформах,
native macOS build и удалённый CI не запускались; соответствующие ограничения
обозначены в release notes. Cross-build, создание пакета и `--version` не
заменяют platform runtime smoke. Подписывание/notarization остаются недоступны
без сертификатов; все результаты UNSIGNED. Raw HTTP spools после crash,
unknown secrets в явно включённых bodies и непрерываемый OS keyring prompt
остаются описанными residual risks. Новые optional features (OpenAPI/OAuth2,
scripts/runners, WS reconnect/H2/H3) не входят в завершённые этапы.
История HTTP/gRPC остаётся в памяти и не является долговременным архивом вызовов.

## Исправления CI на 2026-10-26

По результатам первых Windows/macOS core jobs исправлены платформенные
предположения тестов. Проверки содержимого скачивания, SQLite CRUD и типов файлов
выполняются на всех ОС; точные Unix permissions проверяются вне Windows, где
`os.Stat` не отражает ACL. Lifecycle fixture задаёт `TMPDIR`, `TMP` и `TEMP`,
поэтому Windows response spools попадают в проверяемый временный каталог.
Диагностика missing import сравнивается с каноническим include path после
`filepath.EvalSymlinks`, включая раскрытие Windows 8.3 names.

TLS-тесты получили независимые пятисекундные contexts вместо общего deadline
и 250 ms на загрузку системного trust store. Проверки отказа для недоверенного
CA и неверного имени сервера сохранены; production TLS policy не изменялась.

Дополнительно race detector выявил чтение gRPC trailers до завершения транспорта,
когда client-streaming peer отправляет лишний ответ. `RecvResult` теперь отменяет
транспорт и ждёт `grpc.OnFinish` перед чтением trailers, сохраняя исходный статус
ошибки. Regression fixture оставляет некорректный RPC открытым и проверяет его
завершение и отмену peer без ожидания deadline. Trailers завершённых корректных
RPC по-прежнему обязательны; незавершённый нарушающий протокол peer может их
не передать.

Полные `go test -race ./...`, `go vet ./...` и `git diff --check` прошли.
TLS scenarios повторены пять раз, streaming/cardinality/concurrent observations
— 30 раз с race detector. Windows test binaries пакетов app, importer, storage
и grpcclient прошли полные suites под Wine 10; download/shutdown повторены трижды.
gRPC test binary также скомпилирован для darwin/arm64. Wine и cross-compilation
не заменяют native Windows/macOS runners: повторный удалённый CI после этих
исправлений ещё не выполнялся. Пакеты 0.1.0 из предыдущего этапа не пересобирались
и не включают новое исправление gRPC.

### Повторный macOS CI: диагностика TLS

Повторный core job показал, что увеличение timeout до пяти секунд не устранило
сбой default-roots сценария. В классификаторе были найдены две отдельные проблемы:
не все ошибки native verifier имеют конкретный x509 subtype, а отмена handshake
при закрытии соединения могла перезаписать уже сохранённую причину отказа.
[Darwin verifier в Go](https://go.dev/src/crypto/x509/root_darwin.go) преобразует
лишь часть кодов Security.framework в типизированные x509 errors; остальные
ошибки проверки всё равно оборачиваются TLS в `CertificateVerificationError`.

Добавлена безопасная категория `TLS certificate verification failed` после
более конкретных x509 classifiers. Cancellation/deadline не стирают предыдущую
диагностику; `Dial` сохраняет её до `conn.Close`. Исходный текст сертификата и
платформенной ошибки не раскрывается. Проверка TLS остаётся включённой.

Default-system-roots тест требует именно certificate rejection: один timeout
или общий handshake failure не считается успешной проверкой. Дополнительный
сценарий с пустым явным CA pool сохраняет точную проверку `trusted authority`.
В тестовый self-signed сертификат добавлен непустой Subject/Issuer. По имеющемуся
логу нельзя установить, какой именно Apple status был получен на runner.

Новые credential-boundary regressions воспроизводят generic platform error и
перезапись причины отменой на прежнем коде через Go overlay; после исправления
они проходят, включая проверку отсутствия canary в диагностике. Полные
`go test -race ./...` и `go vet ./...` прошли; TLS integration повторён пять раз.
Windows grpcclient suite прошёл под Wine 10, darwin/arm64 test binary собирается.
Native macOS и повторный удалённый CI после этого исправления ещё не запускались.
