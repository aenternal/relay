# Relay

Локальный desktop API-клиент для HTTP, gRPC и WebSocket. Реализован MVP, подготовлены локальные unsigned packages.
Доступны все четыре типа gRPC RPC и server reflection,
импорт Protobuf, редактор сообщений, HTTP-запросы из React UI через Go,
workspace, коллекции, вложенные папки, вкладки, SQLite, окружения и системный
keychain. Доступны Auto, HTTP/1.0, HTTP/1.1, HTTP/2, h2c и HTTP/3, TLS/mTLS,
авторизация, прокси, multipart, отмена и сохранение ответов.
WebSocket работает через Go с HTTP/1.1 Upgrade: `ws/wss`, text/binary,
ping/pong, закрытие соединения и сохранение ограниченной истории сессии.

В разделе Protobuf можно импортировать файлы/папку `.proto`, добавить include
директории или открыть бинарный `FileDescriptorSet` (`.protoset`, `.pb`, `.desc`).
Зависимости/WKT компилируются локально без `protoc`. Доступны дерево сервисов,
типы и комментарии, создание gRPC-запроса из метода, Strict ProtoJSON/JSON5-like,
Validate, Format и Generate Example. Canonical JSON и hex получаются из настоящего
binary Protobuf в Go с сохранением int64/uint64. Схемы и SHA-256 восстанавливаются
после перезапуска; переимпорт предупреждает об изменении выбранных методов.

Для gRPC выберите endpoint `host:port`, локальную схему и метод либо нажмите
Discover via Reflection. Discovery поддерживает v1 и v1alpha; полученная схема
сохраняется для следующих вызовов и работы после перезапуска. Call передаёт binary
Protobuf через grpc-go по HTTP/2 без generated stubs. Доступны TLS/plaintext,
custom CA/mTLS, SNI/authority, повторные metadata, binary `-bin` values в base64,
gzip, deadline и Cancel. Ответ содержит ProtoJSON/hex, headers/trailers,
status/details, TLS и timing. Вызов подставляет переменные активного окружения;
локальная Validate этого не делает.

Для server-streaming кнопка Start stream отправляет текущее сообщение и начинает
приём ответов. Client-streaming и bidi открываются без отправки: Send message
передаёт очередное тело редактора, Half-close завершает отправку и продолжает
приём. Cancel отменяет оба направления. В bidi следующее сообщение можно
отправить после получения предыдущего ответа в том же потоке. Ctrl/⌘ Enter
открывает поток или отправляет сообщение; Escape отменяет его.

История показывает направления, индексы, время, размер и JSON/hex каждого
сообщения; после завершения остаются status/details и полученные trailers.
Настройки соединения, схема и окружение фиксируются на время потока.
История ограничена 2 000 сообщениями и общим бюджетом UI 32 MiB строк;
ранние превью освобождаются с предупреждением. ACK из UI ограничивает очередь
Go 64 сообщениями / 14 MiB на поток. Закрытие вкладки отменяет RPC, сообщения
и результаты не сохраняются в SQLite.

Для WebSocket задайте URL `ws://` или `wss://`, при необходимости query, headers,
Origin, Cookie и subprotocols, затем нажмите Connect. Send message отправляет
текущий текст или binary в hex/base64/из файла; следующие сообщения используют
то же соединение. Ping проверяет ответ сервера и доступен раз в секунду.
Disconnect выполняет close handshake с code/reason, Abort немедленно прерывает
соединение. Настройки и окружение фиксируются при Connect. Доступны custom CA,
mTLS, insecure TLS и прокси HTTP/HTTPS/SOCKS5; автоматические heartbeat/reconnect
и WebSocket поверх HTTP/2 или HTTP/3 пока не реализованы.

WebSocket timeline показывает направления, timestamps, размеры, ping/pong,
ошибки и close code/reason. Превью ограничено 64 KiB; при перегрузке старые события
освобождаются со счётчиком пропусков, чтение socket и control frames продолжается.
После закрытия нажмите «Сохранить сессию»: по умолчанию сохраняется только metadata.
«Включить payloads в историю» добавляет обработанные ограниченные превью.
URL query и чувствительные headers исключаются из сохранённой истории всегда.
До 16 сессий / 32 MiB восстанавливаются после перезапуска; их можно открыть и
удалить в «Сохранённые сессии WebSocket». История хранит только эти превью,
без отдельного архива полных сообщений. Сохранение тела редактора включается отдельно.

«Импорт / экспорт проекта» переносит одно рабочее пространство в JSON
`relay-project/v1` вместе с HTTP/gRPC/WebSocket definitions, окружениями и
самодостаточными Protobuf-схемами. Импорт создаёт новое пространство, сохраняя
существующие данные и настройки. Тела исключены по умолчанию; включение задаётся
отдельно при экспорте и импорте. Secret values, keyring references, выбранные
файлы и response history не переносятся. После импорта повторно задайте секреты
и выберите TLS-файлы. В desktop доступны native file dialogs; server mode
использует JSON file input/download. Подробности: [native project](docs/native-project.md).

Доступны Postman Collection v2.1 и Insomnia JSON export v4: preview показывает
количество сущностей и предупреждения перед применением. Поддерживается
документированное HTTP-подмножество, scripts/tests/flows не исполняются.
Версии и пропускаемые поля описаны в [матрице импорта](docs/import-compatibility.md).

Поиск коллекций учитывает имя запроса, URL/endpoint, HTTP/RPC method и имена
родительских коллекций/папок; доступны фильтр протокола и сортировка по имени.
«История запросов» показывает последний доступный результат каждого запроса
текущего процесса с фильтрами протокола/состояния и сортировкой. Отдельная
сохранённая WebSocket history остаётся в панели ответа. Ctrl/⌘ K переводит фокус
в поиск, Ctrl/⌘ Tab и Shift переключают открытые вкладки, Ctrl/⌘ , открывает
настройки. Ширину списка и высоту редактора можно менять мышью либо стрелками
на разделителе; Shift увеличивает шаг, Home/End задают пределы.

Также работают независимые от UI Go-пакеты: HTTP/1.0, HTTP/3 через QUIC,
импорт `.proto` с вложенными зависимостями/WKT, JSON5-like → dynamic Protobuf,
gRPC unary и streaming без generated client. Текущий статус и ограничения:
[docs/progress.md](docs/progress.md), [docs/protocols.md](docs/protocols.md).

Требуется Go **1.26.10**. Проверки используют локальные серверы и временные TLS-сертификаты:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Основное приложение (Go 1.26.10, Node 24.19.0; Linux native требует GTK3/WebKitGTK 4.1
development packages, либо GTK4/WebKitGTK 6 без тега `gtk3`):

```sh
cd frontend
npm ci
npm run build
cd ../cmd/desktop
go run -tags gtk3 .       # Linux GTK3
# go run .               # Windows/macOS; Linux GTK4
```

Запуск без GTK через настоящий Wails bridge в браузере (bash, после сборки frontend):

```sh
cd cmd/desktop
CGO_ENABLED=0 WAILS_SERVER_HOST=127.0.0.1 WAILS_SERVER_PORT=9246 go run -tags server .
# Открыть http://127.0.0.1:9246
```

Это локальный режим разработки; он не предназначен для публикации в сети.
Приложение принимает только loopback bind; публичный proxy или туннель к bridge
не поддерживается. Встроенные assets имеют Content Security Policy, scripts
и HTML из ответов не выполняются. Production build отключает devtools и
автоматическое логирование asset requests.
По умолчанию профиль — `os.UserConfigDir()/relay`, база — `relay.db`.
`RELAY_DATA_DIR` задаёт отдельный каталог профиля. Одновременный запуск двух
процессов с одним профилем запрещён kernel lock. Коллекции, настройки и вкладки
восстанавливаются после перезапуска. Тела definitions остаются в памяти, пока
пользователь не включит их сохранение. Полный HTTP response временно хранится
в private spool для последующего Save; UI preview может отличаться от исходного
ответа. Секреты не возвращаются в UI и хранятся только через
системный keychain; при его недоступности сохранение секретов отключено.

Версия, commit, платформа, Go toolchain и статус **UNSIGNED** доступны в настройках
и через `relay --version`; эта команда не открывает профиль или GUI. Локальный
release tool создаёт Linux GTK3 `.tar.gz`/`.deb`, Windows portable `.zip` и на
native macOS runner — архив с `.app`. Пакеты проверяются по ожидаемой версии,
SHA-256 и содержимому архива без запуска. Подпись, notarization, публикация и
автоматическая установка обновлений не выполняются.

Сборка и проверка пакетов описаны в [релизном процессе](docs/releases.md).
Локальные артефакты версии 0.1.0 находятся в `dist/release/0.1.0/`: Linux GTK3
tar.gz/deb и Windows portable ZIP с manifest/SHA-256. Пакеты не публиковались.
При проблемах с запуском, TLS, reflection, import или keychain используйте
[troubleshooting](docs/troubleshooting.md). Границы защиты и остаточные риски
описаны в [модели угроз](docs/security.md), измерения памяти и shutdown —
в [performance](docs/performance.md).

Проверки frontend и основного приложения:

```sh
cd frontend
npm run build
npm test
npm run lint
npm run format:check
npx playwright install chromium
npm run test:e2e           # Временная БД, реальный Wails bridge, перезапуск процесса
cd ../cmd/desktop
CGO_ENABLED=0 go test -tags server ./...
```

Обновить TypeScript bindings после изменений Go-сервиса (из корня):

```sh
go run ./tools/bindings
```

Нативное окно Linux проверено с GTK 3.24.52 и WebKitGTK 2.52.6. Проверки keychain
используют внедряемый тестовый vault и не изменяют системные секреты пользователя.
Windows/macOS runtime и реальные OS keychain ещё требуют проверки на своих ОС;
Windows amd64 binary собирается. Упакованный production GTK3 binary выполнил
реальный WebSocket echo; настройки показали версию 0.1.0 и статус UNSIGNED.
Полные Go race/vet, 139 UI tests и девять E2E проходят. Govulncheck не нашёл
уязвимых вызываемых или импортируемых packages; один module-only advisory
относится к неимпортируемому SSH-коду. npm audit сообщает ноль vulnerabilities.
Решения о хранении и очистке секретов: [ADR 002](docs/decisions/002-storage-and-secrets.md).

В HTTP-редакторе Send (Ctrl/⌘ Enter) отправляет текущий draft, Escape отменяет
запрос. Ответ показывает фактический протокол, статус, headers/trailers, TLS,
редиректы и timing. HTML отображается как текст. Превью по умолчанию ограничено
5 MiB, полное тело пишется во временный файл до 256 MiB; лимиты настраиваются.
Выбор файлов и сохранение полного ответа доступны через нативные диалоги.
Пароли/токены вводятся на вкладке Auth либо через `{{secret}}` из окружения;
прямой ввод остаётся только в памяти. Выбранные файлы нужно повторно выбрать
после перезапуска. Подробная матрица: [docs/protocols.md](docs/protocols.md).

Проверка Protobuf является офлайн-операцией: она использует текст редактора без
подстановки environment/keychain secrets. Для 64-битных чисел рекомендуются
строки, например `"9223372036854775807"`; числовые литералы также обрабатываются
без округления. Ошибки содержат позицию и путь поля без чувствительного значения.
В проекте сохраняется самодостаточный descriptor set с комментариями, а исходные
пути файлов остаются в Go. Для переимпорта исходники выбираются повторно.
Поддержаны proto3, проверенная матрица proto2 и Edition 2023; Edition 2024,
ZIP-импорт и remote imports пока не поддержаны.

При gRPC `Unavailable` проверьте endpoint и режим TLS/plaintext. Для self-signed
сервера выберите доверенный CA PEM; server name должен соответствовать сертификату.
Insecure явно отключает проверку и показывает предупреждение. Если reflection
возвращает `Unimplemented`, импортируйте `.proto`/descriptor set, выберите метод
и отправьте запрос с локальной схемой. Для `Unauthenticated` проверьте metadata,
например `authorization: Bearer {{token}}`, и активное окружение.

gRPC по умолчанию использует общий deadline 30 секунд, connect timeout 10 секунд
и лимиты send/receive 4 MiB. В Go одновременно выполняются до четырёх операций;
результаты ограничены 64 MiB, в UI — 32 MiB строк. Известные секреты маскируются,
содержащие их binary previews скрываются. System keychain prompt не прерывается
контекстом: UI остаётся отзывчивым, worker может ждать возврата системного вызова.

CLI-прототип позволяет проверить ядро до появления основного UI:

```sh
# Список методов из локальной схемы, без protoc.
go run ./cmd/spike proto -I testdata/proto -proto relay/v1/echo.proto

# Канонический ProtoJSON и настоящий binary Protobuf (hex).
printf "{text: 'hello', sequence: 9223372036854775807,}" | \
  go run ./cmd/spike proto -I testdata/proto -proto relay/v1/echo.proto \
  -method /relay.test.v1.EchoService/Unary -json5 -body -

# Вызов совместимого gRPC-сервера (с TLS по умолчанию, при необходимости -ca root.pem).
go run ./cmd/spike grpc -endpoint localhost:44044 \
  -I /path/to/protos -proto service.proto \
  -method /package.Service/Method -body request.json

# Для bidi повторите -body для каждого сообщения; приём работает параллельно отправке.
# -plaintext включает незашифрованный gRPC; -insecure отключает проверку сертификата
# только для текущего запуска и печатает предупреждение.

# Явный транспорт без fallback, -http-body принимает файл либо stdin (-).
go run ./cmd/spike http -version 1.0 -url http://localhost:8080/
go run ./cmd/spike http -version 3 -url https://localhost:8443/ -ca root.pem
```

По умолчанию CLI имеет общий таймаут 15 секунд (`-timeout`), а HTTP-клиенты —
лимиты тела 5 MiB и заголовков 64 KiB. HTTP-ответ CLI представляет тело в base64,
чтобы не терять бинарные данные. Для остановки запроса используйте Ctrl+C.

Прежний Wails smoke-прототип этапа 0 изолирован отдельным модулем; это проверка Go→UI событий,
подтверждения доставки, ограничения очереди и отмены, а не основной интерфейс:

```sh
cd spikes/desktop
go run -tags gtk3 .              # Linux: GTK3 + WebKitGTK 4.1 development packages
# go run .                      # Windows/macOS; Linux по умолчанию требует GTK4/WebKitGTK 6

# Headless-проверка реального Wails bridge в браузере, без GTK.
CGO_ENABLED=0 go test -tags server ./...
go test -race service.go service_test.go
npm ci
npx playwright install chromium
npm test

# Ручной запуск того же server mode на loopback:
CGO_ENABLED=0 WAILS_SERVER_HOST=127.0.0.1 WAILS_SERVER_PORT=9245 go run -tags server .
```

Версии зависимостей, обоснование решений и ссылки на API:
[ADR 001](docs/decisions/001-stack.md). Исходный пример Onyx сохранён в
[examples/onyx/healthcheck.go.txt](examples/onyx/healthcheck.go.txt).
