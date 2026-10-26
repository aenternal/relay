# Архитектура Relay

## Application и desktop UI: этапы 1–7

Зависимости направлены от desktop adapter к Go-core; domain, storage, security
и transport не импортируют Wails. Основная цепочка HTTP:

```text
React / Zustand / CodeMirror
    └── generated Wails bindings
          ├── app.Service ── storage / security
          │     ├── httpclient/engine
          │           ├── net/http: Auto / HTTP/1.1 / HTTP/2 / h2c
          │           ├── http10.Transport: TCP / TLS HTTP/1.0
          │           └── quic-go/http3: QUIC / TLS HTTP/3
          │     └── protobuf/importer / registry / jsoncodec
          │           └── protocompile / dynamicpb / protojson / proto
          │     └── grpcclient: grpc-go Invoke / server reflection v1 / v1alpha
          │     └── websocketclient: coder/websocket / HTTP/1.1 Upgrade
          │     └── project: native / Postman v2.1 / Insomnia v4, offline transfer
          └── desktop.FileService ── native file dialogs
                    └── opaque file grants / explicit response and project save
```

- `internal/domain`: модели определений, окружений, настроек, ProtoSchema,
  HTTPExecution, GRPCExecution и WebSocketExecution;
  PEM private keys и пути выбранных файлов в эти модели не входят.
- `internal/storage`: SQLite snapshot transactions с нормализованными связями,
  migrations v1/v2/v3/v4, revision checks, outbox удаляемых secret references
  и отдельная ограниченная таблица явно сохранённых WebSocket sessions.
- `internal/security`: OS keyring, ограниченная подстановка переменных,
  отдельные wire/redacted representations. MemoryStore внедряется только в тестах.
- `internal/app`: CRUD, snapshots окружений, inheritance auth, подготовка запроса,
  execution lifecycle, preview budgets, приватные временные response files,
  импорт/инспекция схем, локальная валидация Protobuf, unary/stream RPC, reflection
  и долгоживущие WebSocket sessions.
- `internal/project`: ограниченный JSON guard, native project codec, offline
  descriptor verification, Postman/Insomnia HTTP parsers и privacy filtering.
  Пакет не зависит от Wails, SQLite или системного keychain.
- `internal/transport/httpclient/engine`: streaming HTTP client с явными версиями,
  TLS/proxy/redirect/cookie/decompression policies и владением соединениями.
- `internal/transport/websocketclient`: ws/wss HTTP/1.1 Upgrade, text/binary,
  control frames, ограниченные сообщения и bounded graceful close.
- `internal/desktop/instancelock`: kernel lock профиля до открытия БД и keychain.
- `cmd/desktop`: отдельный Wails module, нативные dialogs, shutdown context,
  graceful WebSocket shutdown, очистка HTTP/gRPC resources и закрытие SQLite.
- `frontend`: npm React/TS/Vite и отдельный Go assets module для embed.
  Такое разделение исключает обход node_modules при headless Go tests.
- `tools/bindings`: генерация сохранённых TypeScript interfaces/services
  закреплённым Wails CLI в `frontend/src/bindings`.

Нативное Linux-окно проверено с GTK 3.24.52 и WebKitGTK 2.52.6. Server mode
с браузером используется для integration/E2E, включая Go transport: target API
не вызывается браузерным fetch. Native Windows/macOS runtime и реальные keychain
на всех платформах остаются отдельными проверками.

## Перенос проектов и навигация

`ExportProject`, `PreviewProjectImport` и `ImportProject` используют общий budget
offline tools: два параллельных вызова и deadline 30 секунд. Обработка и
восстановление descriptors происходят вне mutex persistence. JSON guard
проверяет размер 32 MiB, глубину 32, до 200 000 tokens и duplicate properties
до typed decoding. Native version фиксирована; Postman v2.1 и Insomnia JSON v4
имеют отдельные parsers, общий промежуточный Document и одинаковый privacy path.
Никакой импорт не исполняет scripts, не подключается к API и не читает файлы
по путям из документа.

Preview возвращает только ограниченный отчёт. Apply повторно проверяет файл,
заменяет IDs и все внутренние references, проверяет graph ownership, затем
под storage mutex загружает актуальную ревизию и дополняет snapshot новым
workspace. Existing preferences/UI и secret references остаются на месте.
Definitions и descriptor artifacts фиксируются одной CAS-транзакцией SQLite;
ошибка откатывает весь перенос. Отдельные native file commands получают пути
только через OS dialog, ограничивают чтение и сохраняют экспорт через private
temporary sibling и rename. Server mode использует browser file input/download.
Подробности формата и privacy policy: [native-project.md](native-project.md),
подмножества внешних форматов: [import-compatibility.md](import-compatibility.md).

Поиск и сортировка дерева вычисляются в UI из definitions, без новой схемы БД.
Поиск учитывает имена ancestor folders/collections, адреса и HTTP/RPC methods;
дерево сохраняет контекст совпадений при фильтре протокола. Recent executions
строится из последних доступных результатов существующих запросов текущего
процесса и не добавляет долговременную HTTP/gRPC history. Разделители панелей
доступны через pointer capture и keyboard; глобальные shortcuts учитывают
активный view и модальные окна. Импорт сериализован с UI autosave; после
применения store получает новый snapshot, сохраняя существующие drafts/results.

## Жизненный цикл HTTP

`StartHTTPRequest` принимает snapshot текущего draft, кратко загружает state
под mutex хранения и выбирает активное окружение workspace. Keychain, file reads
и сеть выполняются вне storage mutex. HTTP definition проверяется до подключения:
URL/header syntax, переменные, лимиты, body format, grants и TLS/proxy options.
JSON5-like нормализуется до подстановки JSON strings; int64 не проходит через
float64. Auth inheritance выбирает ближайшую папку, затем коллекцию.

После проверки создаются execution ID и worker с контекстом Service и общим
deadline. Возврат из Wails-вызова этот контекст не отменяет. Frontend хранит
result отдельно от сохранённого определения, опрашивает только running requests
и использует generation IDs для изоляции вкладок и поздних результатов.
Cancel отменяет контекст, освобождает соединение и оставляет полученную часть
ответа с headers/status/timing. Shutdown отменяет workers и ждёт их завершения.

Engine.Do возвращает live response Body. Worker пишет поток в mode-0600 spool,
считает полученные байты и удерживает только ограниченное preview в памяти.
Лимит download проверяется дополнительным байтом; он действует после gzip /
deflate decompression. При превышении остаётся явный failed/partial result.
Body и client всегда закрываются, включая отмену, ошибку и early return.
Timings собираются через httptrace там, где транспорт даёт соответствующие hooks.

Defaults application service: 30 секунд общего deadline, 10 секунд connect/TLS,
30 секунд headers, 5 MiB preview и 256 MiB download. Максимумы: 32 MiB preview,
1 GiB download/upload, восемь активных запросов и 16 результатов. Retained
previews ограничены общим бюджетом 64 MiB с запасом на base64; временные
encoding buffers не входят в этот retained budget. При нехватке места удаляется
старейший завершённый result и его spool. Отдельной суммарной disk quota нет;
место ограничено числом результатов и индивидуальными download limits.

Request URL/headers ограничены 64 KiB, transport response headers — 1 MiB.
Редакторский body и form/multipart text — до 4 MiB. Binary и multipart files
передаются потоком; HTTP/1.0 получает Content-Length, вычисленный из multipart
headers/boundary и размеров файлов, без загрузки файла в RAM. Неизвестный размер
streaming upload для HTTP/1.0 отклоняется. Decompression ограничена восемью
слоями Content-Encoding.

## Файлы, секреты и границы доверия

Нативный `FileService` открывает OS dialog и вызывает package function
`RegisterHTTPFile`. Webview получает только случайный grant ID, имя и размер.
Grant map живёт в Service; JS не может зарегистрировать произвольный путь.
Перед открытием проверяются regular file, identity, размер и modification time;
изменённый файл нужно выбрать заново. Ограничение — 256 grants за процесс,
TLS PEM — до 1 MiB. Grants не сохраняются в SQLite.

`SaveHTTPResponse` вызывается только после native save dialog. Оно копирует
полученные байты через приватный временный файл рядом с выбранным destination
и заменяет destination после успешного завершения. Forget, eviction и shutdown
удаляют spools. Файлы ответов не являются долговременной историей; аварийное
завершение процесса может оставить временный файл, как и у обычного temp storage.

Responses и resolved secrets не записываются в application logs. HTTP/gRPC
responses не сохраняются в SQLite. WebSocket имеет отдельное явное сохранение
завершённой session: metadata по умолчанию, очищенные ограниченные previews
только после отдельного payload opt-in. Сохранённые auth credentials и sensitive headers/query
содержат environment references; secret values находятся в OS keychain.
Raw/form/multipart text сохраняется только при body opt-in. Autosave удаляет
volatile body/grants из persistent copy, сохраняя текущий draft в памяти вкладки.
В UI нет browser storage для запросов или токенов. SQLite/keychain consistency
и recovery outbox описаны в `decisions/002-storage-and-secrets.md`.

Перед передачей UI маскируются известные secret values, sensitive headers
и все URL query values. Это касается response preview, redirect/TLS metadata
и diagnostics. Не выводится исходный текст ошибок peer/proxy/vault. Safe error
classes дают контекст DNS/connect/TLS/protocol/proxy/decompression/limits без
credential values. Ответы отображаются текстом, HTML не исполняется.

Сохранение ответа — явное действие пользователя с исходным полученным телом,
включая секреты, скрытые в preview; UI показывает предупреждение при redaction.
При decompression сохраняется распакованный поток. При cancel/download limit
сохранение содержит только полученную часть, а не обещанный сервером полный body.

Redirect origin сравнивается по scheme/hostname/port. На другом origin удаляются
credential headers, Referer и защищённые query keys; это правило сохраняется
для всей оставшейся цепочки. mTLS redirect на другой origin блокируется,
а client certificate не попадает во внешний TLS handshake HTTPS proxy.
TLS verification включена по умолчанию; insecure является opt-in одного request.
Cookie jar общий для Service и существует только в памяти процесса.

## Импорт Protobuf и редактор сообщений

Раздел Protobuf хранит схемы в workspace и показывает файлы, packages, сообщения,
поля и дерево сервисов. RPC methods имеют полный путь `/package.Service/Method`
и признаки client/server streaming. В gRPC draft выбираются схема и метод,
формат JSON/JSON5-like, безопасный шаблон и локальная проверка тела. Unary RPC
выполняется через Go; streaming controls остаются этапом 5.

`ImportProtoSchema` принимает виртуальные `.proto` sources и root filenames либо
base64 бинарного FileDescriptorSet. Этот путь компилирует только переданные
тексты и bundled WKT; он не читает файловую систему и не использует сеть.
Нативный `FileService` отдельно выдаёт opaque selection IDs после выбора файлов,
папки и include directories. Только package function `ImportProtoPaths` получает
physical paths и вызывает filesystem resolver. Paths не входят в bridge DTO;
при ошибках остаются virtual filename, позиция и подсказка о include directories.
ZIP и загрузка удалённых схем не реализованы.

`protocompile` строит отдельный registry с полной dependency closure. Standard
WKT — fallback только для отсутствующего локального import; конфликтующие
пути и symbols, import cycles, отсутствующие зависимости и symlink escape
отклоняются. Directory import обходит `.proto` files с ограничением количества
entries и roots. Бинарный descriptor set может не содержать стандартные WKT:
resolver добавляет их офлайн; остальные imports должны присутствовать.

Migration v3 добавляет `proto_schemas` с workspace foreign key, JSON metadata
и отдельным `descriptor_set_base64`. В artifact включаются зависимости и source
information; обычные JSON snapshots раскрывают только metadata. Поле
`ProtoSchema.DescriptorSetBase64` имеет `json:"-"`; storage явно сохраняет и
восстанавливает его отдельной колонкой. SHA-256 проверяется при загрузке и
восстановлении registry. Исходные папки после импорта для работы не нужны.
GRPCDefinition сохраняет SchemaID, method и BodyFormat; body требует opt-in.

Reimport сохраняет schema ID и атомарно заменяет artifact. Если выбранный в
сохранённом запросе RPC исчез либо изменились связанные messages, streaming,
presence или edition semantics, возвращается review notice. Это проверка
изменений, а не полная гарантия wire compatibility. Сравнение выполняется вне
storage mutex с кешем methods/message shapes; при concurrent replacement
проверяются прежние checksum и ImportedAt. Удаление схемы очищает SchemaID
связанных запросов, сохраняя method и разрешённое к сохранению тело.

`InspectProtoSchema` возвращает registry metadata; `InspectProtoMethod` — input,
output, RPC kind и template. Шаблон не копирует source defaults, комментарии
или ранее введённое тело, выбирает одну ветку oneof, оставляет lists/maps пустыми
и останавливает optional recursion. Required recursive graph без конечного
примера даёт явную ошибку.

`ValidateProtoMessage` выполняет JSON5-like normalization → strict ProtoJSON
validation → dynamicpb → deterministic binary wire → binary decode → canonical
ProtoJSON. Числа сохраняются без float64, int64/uint64 возвращаются строками по
правилам ProtoJSON. Диагностика содержит line/column/path и безопасную причину,
без значений полей, map keys и Any type URLs из пользовательского тела. Результаты
валидации и wire previews не сохраняются. Это локальная операция: environment
variables не подставляются и keyring не читается; `{{variable}}` внутри строк
остаётся буквальным текстом с предупреждением.

Лимиты Protobuf: 512 файлов в dependency closure, 256 root files, 64 include
directories, 8192 entries при обходе папки, 2 MiB на source и 16 MiB на sources
в сумме или descriptor set. Snapshot допускает 64 схемы и 32 MiB artifacts.
Metadata показывает до 256 root names; полный artifact сохраняется при большем
числе независимых roots descriptor set. Editor input и wire — до 4 MiB,
JSON/Protobuf recursion — 100, canonical JSON — до 8 MiB; при большом отступном
формате используется compact JSON. Template — до 256 KiB, 2048 полей и depth 12.
Одновременно работают две операции импорта/инспекции/валидации; при занятом
бюджете новый вызов сразу возвращает busy error. Deadline — 30 секунд, shutdown
Service отменяет active work. Descriptor processing не удерживает storage mutex.

Поддержаны proto2, proto3 и edition 2023. Более новые editions отклоняются явно.
Матрица Go-core включает scalar types, enum, optional/required, repeated/map,
oneof, nested/recursive messages, extensions, WKT и dynamic Any. Proto1 MessageSet
и полный синтаксис JSON5 не заявлены. Unary RPC и reflection подключены
к основному UI; streaming controls остаются этапом 5.

## Жизненный цикл gRPC unary и reflection

`StartGRPCUnary` и `StartGRPCReflection` сразу возвращают running execution ID.
Worker получает snapshot определения, контекст Service и общий deadline;
завершение bridge-вызова его не отменяет. Загрузка workspace кратко использует
storage mutex; keyring, восстановление registry, подготовка тела и сеть работают
вне него. `GetGRPCExecution` возвращает копию ограниченного результата,
Cancel отменяет контекст, Forget разрешён после завершения. Shutdown отменяет
все workers, ждёт их выхода и освобождает соединения.

Unary не запускает reflection автоматически: выбранный schema ID восстанавливает
registry из проверенного artifact. Полный RPC path и input/output descriptors
берутся из registry; streaming method отклоняется до подключения. JSON5-like
нормализуется, переменные активного окружения подставляются только в JSON strings,
затем ProtoJSON создаёт dynamic message. Неверные metadata/JSON/grants не доходят
до RPC. `grpc-go ClientConn.Invoke` передаёт binary Protobuf по HTTP/2 без stubs.

TLS включён по умолчанию. Native CA/cert/key grants переиспользуют HTTP file
service; SNI и authority настраиваются отдельно. Plaintext сохраняет TLS profile
как неактивный и показывает предупреждение. Проверка сертификата отключается
только явным insecure option. Proxy environment не используется gRPC adapter.
Metadata допускает повторяющиеся lowercase names; `-bin` принимает base64,
который декодируется в raw bytes до вызова grpc-go. Transport-reserved names
отклоняются. Доступно request compression none/gzip.

Результат содержит status code/message, initial headers, trailers, connection
и total timing, фактическую TLS state и bounded status details. Известные detail
типы разрешаются из локальной схемы; стандартные google.rpc types имеют встроенный
fallback. Неизвестный Any показывает type URL и безопасные raw bytes с пояснением.
Response wire — точные Protobuf bytes после gRPC decompression, скопированные
из входного buffer codec; порядок полей и повторные singular tags сохранены.
ProtoJSON сохраняет int64 strings. Execution и wire previews не пишутся в SQLite.

Известные секреты маскируются в JSON, metadata, status, TLS и errors. Binary preview
не показывается, если содержит известный секрет либо JSON redaction обнаружила
чувствительное значение. JSON, превышающий лимит после redaction, опускается
с предупреждением. Общее маскирование выполняется за один проход, не обрабатывает
повторно собственные markers и ограничивает размер выходного текста.

Reflection вызывается явно и использует тот же TLS/metadata profile: v1,
fallback на v1alpha только при Unimplemented. Полная dependency closure проверяется
и сохраняется как обычная descriptor schema с checksum; ошибочная или содержащая
известные секреты схема не сохраняется. В пределах процесса bounded cache
connection-profile fingerprints обновляет прежний schema ID при повторном
discovery; fingerprints и credentials не сохраняются. Сохранённый artifact
работает после перезапуска без reflection и исходного сервера. Если reflection
недоступен, UI предлагает импортировать `.proto` или descriptor set.

Defaults: 30 секунд общего deadline, 10 секунд connect, 4 MiB send/receive;
настраиваемые message limits не превышают 4 MiB, metadata — 64 KiB. До четырёх
активных gRPC операций, 16 retained results и 64 MiB общего Go result budget
с предварительным резервированием JSON/base64/details; временные schema/codec
buffers в этот retained budget не входят. Старейшие завершённые results
вытесняются первыми. UI отдельно удерживает до 16 завершённых results и 32 MiB
строк, сохраняя активные операции. ProtoJSON ограничен 8 MiB, details — 1 MiB
в сумме и 128 elements, protobuf recursion — 100. Reflection: до 512 файлов,
16 MiB descriptors, 2048 services/requests и 30 секунд на discovery; session cache
ограничен 256 fingerprints.
Binary descriptors проверяются до protobuf unmarshal: общий структурный бюджет
100 000 единиц работы, глубина 100. Reflection суммирует работу по всем полученным
файлам, включая повторы; тот же guard используется для descriptor import и перед
сохранением compiled artifacts.

OS keyring upstream API синхронный: контекст проверяется до и после системного
вызова, но не прерывает открытый системный prompt. Start/Cancel не блокируют UI;
worker и shutdown могут ожидать возврата этого вызова. Это отличается от отмены
TCP/TLS/RPC, которые используют прерываемый context.

## Жизненный цикл gRPC streams

`StartGRPCStream` создаёт отдельное соединение и долгоживущий context Service
с общим deadline. Тип server/client/bidi определяется выбранным method descriptor,
генерация stubs не требуется. Initial state — `connecting`; после подготовки
connection profile и открытия stream — `open`. Server-streaming отправляет body
один раз и делает half-close автоматически. Client-streaming и bidi открываются
без отправки body; каждое сообщение передаётся отдельным `SendGRPCStreamMessage`.
`HalfCloseGRPCStream` закрывает только send direction (`half_closed`); receive
продолжается до EOF/ошибки. Любой terminal execution получает `closed`.

Send и receive работают независимо на одном RPC. В приложении одновременно
допускается один Send либо HalfClose на stream; повторная bridge-команда получает
busy error, а не становится в неограниченную очередь. До readiness и после
half-close/terminal такие команды отклоняются. Ошибка JSON/ProtoJSON не отправляет
сообщение и не закрывает исправное соединение. Snapshot schema, endpoint,
TLS/metadata и environment остаётся неизменным для этого stream. Последующие
JSON/JSON5-like bodies подставляют переменные только в Go. Keyring значения
кэшируются при первом использовании; суммарный объём кэшированных raw secret
values ограничен 4 MiB. Новые известные секреты защищены отдельным stream mutex
и маскируют новые сообщения, status, metadata, errors и ещё не ACK-нутые previews.
Уже подтверждённая UI история хранится отдельно от Go-очереди.

В Go хранится очередь до 64 сообщений и 14 MiB preview strings. На stream
предварительно резервируются 16 MiB общего 64 MiB gRPC retained budget: очередь
плюс 2 MiB на status/details/metadata. Временные dynamic messages, codec buffers,
schema и keyring snapshot учитываются отдельно от retained result budget.
При полной очереди producer ждёт ACK; этот backpressure ограничивает дальнейшее
чтение receive direction. Cancel/deadline прерывают ожидание и сетевые операции.
Счётчики sent/received отражают реальные успешные операции, в том числе последнее
сообщение, preview которого ещё ожидало места при Cancel.
Если final status ещё не прочитан, закрытие делает ограниченный best-effort
drain buffered DATA: до 128 дополнительных сообщений с порогом 4 MiB wire.
Порог проверяется между сообщениями; с последним сообщением объём меньше 8 MiB
при максимальном receive limit 4 MiB.
Отброшенные previews отмечаются предупреждением и учитываются в received count;
неполученные final trailers/status не обещаются.

`PollGRPCStream(id, afterSequence)` возвращает execution snapshot, до 32 сообщений
и примерно 1 MiB strings за вызов; одно большее сообщение допускается отдельно.
Общий sequence нумерует оба направления, index — сообщения каждого направления.
`AckGRPCStream` явно освобождает подтверждённый префикс; повторный ACK безопасен,
ACK за пределами уже доставленного Poll отклоняется. Poll без ACK повторяет
доставку, terminal state не мешает дочитать очередь. Pending terminal queues
не вытесняются новым запросом: сначала UI должен ACK-нуть их либо явно Forget.
Shutdown отменяет stream, ждёт receive/header worker и активную send-команду,
затем освобождает соединения и transient records.

Входящий wire preview содержит точные Protobuf bytes из codec после decompression,
с исходным порядком полей и duplicate singular tags, без gRPC frame. Исходящий
preview — отдельная сериализация отправленного dynamic message: порядок map-полей
может отличаться от сериализации grpc-go. Оба направления показывают canonical
ProtoJSON и скрывают binary, если обнаружен известный секрет. История в UI имеет
отдельный лимит на stream и общий бюджет 32 MiB строк вместе с unary results;
отрисовывается окно сообщений. Выполнения и stream messages не пишутся в SQLite.

## Жизненный цикл WebSocket

`ConnectWebSocket` немедленно возвращает ID в состоянии `connecting` и запускает
worker на context Service. Worker фиксирует definition, выбранное окружение,
headers/query/subprotocols, TLS и proxy profile; завершившийся bridge-вызов
соединение не отменяет. TLS PEM выбираются теми же нативными opaque grants, что
и для HTTP/gRPC. На `ws://` сохранённый TLS profile не активируется и grants
не читаются. На `wss://` verification включена по умолчанию; доступны custom CA,
SNI, mTLS и отдельный insecure opt-in. Proxy none/environment/custom задаётся
явно, редиректы не выполняются, HTTP/2 и permessage-deflate не включаются.

После HTTP/1.1 Upgrade состояние становится `open`; доступны Send, Ping,
Disconnect и Cancel. Send text требует UTF-8; binary принимает hex/base64
или один нативный file grant. Неактивные поля composer не участвуют в отправке.
Hex допускает разделители ASCII space/tab/CR/LF; префикс `0x` и другие символы
отклоняются. Каждый отдельный Cookie value из handshake учитывается как секрет,
включая ответное echo без имени cookie.
Environment фиксируется на всё соединение. Шаблоны последующих сообщений
разрешаются только в Go, keyring значения кэшируются при первом использовании.
Окружение ограничено 1024 переменными и 1 MiB значений/имён/ссылок; raw keyring
cache — 256 KiB на connection. Это ограничивает retained snapshot долгой session.
Новые известные секреты маскируют последующие и ещё не ACK-нутые события, а также
отдельный history ring для будущего сохранения. Уже показанная история UI может
содержать текст, который пришёл до первого использования соответствующего секрета.

Одновременно выполняется один Send либо Ping; следующий такой bridge-вызов
получает busy error. Reader работает независимо от writer и UI. Ошибка формата,
изменённый файл или превышение send limit не закрывают исправное соединение.
Connect/write defaults — 10 секунд; read timeout по умолчанию отключён, при
включении ограничивает ожидание следующего полного data message. Лимит одного
сообщения — 1 MiB по умолчанию, до 4 MiB. Текстовые templates и resolved strings
ограничены 4 MiB; literal binary допускает до 8 MiB hex либо эквивалентный base64
без шаблонов, чтобы отправить полные 4 MiB. Файлы читаются целиком до message
limit, WebSocket framing/fragmentation остаётся обязанностью транспорта.

Reader всегда продолжает чтение, даже если UI не подтверждает события: иначе
поток data заблокировал бы pong и close. Go queue ограничена 64 events / 8 MiB;
старые события вытесняются, а `DroppedEvents` делает потери видимыми. Callback
ping/pong использует неблокирующий mutex TryLock; при занятом lock notification
также учитывается в dropped count. Payload control frames в историю не попадает,
сохраняются направление и число байтов, autopong выполняет transport.
Отдельный history ring ограничен 2000 events / 6 MiB, его вытеснения учитываются
в `OmittedEvents` при Save. Текстовый preview — до 64 KiB и корректной UTF-8
границы; binary preview — base64 первых 64 KiB, исходный размер и truncated flag
показываются отдельно. Binary с известным секретом скрывается полностью.

`PollWebSocket(id, afterSequence)` выдаёт execution snapshot, до 32 событий и
примерно 1 MiB preview strings. ACK освобождает только доставленный Poll prefix;
повторный ACK безопасен, будущий sequence отклоняется. Terminal очередь можно
дочитать. Pending terminal queues не вытесняются автоматически при запуске
следующего соединения; явный Forget удаляет их. Admission: четыре active
connections, 16 retained records и 64 MiB retained budget; до Dial резервируются
16 MiB на session (8 MiB queue + 6 MiB history + 2 MiB overhead). Временные
входные сообщения, codec buffers и bridge encoding учитываются отдельно.

Disconnect переводит session в `closing` и запускает graceful close вне
bridge-вызова, с force-close не позднее секунды. Cancel сразу отменяет context
и закрывает физическое соединение. Реальный peer close имеет `closed`, code,
reason и `CleanClose`; потеря TCP, read timeout и локальная protocol/size failure
получают `failed` с `CleanClose=false` (1006, 1007 или 1009). Полученные события
и handshake status/headers остаются доступны. Shutdown сначала посылает 1001
активным соединениям, затем force-closes transport и ждёт reader/commands;
после этого desktop отменяет общий Service context. Синхронный OS keyring prompt
остаётся ограничением upstream API: его возврата worker/shutdown могут ждать.

Сохранение возможно только после terminal state, независимо от ACK. По умолчанию
оно исключает event text/base64, close reason и handshake headers. Payload opt-in
сохраняет только повторно очищенные previews из history ring; raw payload archive
и replay не реализованы. Snapshot берётся под mutex, повторное очищение и JSON
encoding выполняются снаружи, чтобы не задерживать другие reader/control frames.
Существующие `[REDACTED]` markers повторно не маскируются. При JSON escaping
старые events дополнительно удаляются до лимита записи 8 MiB с запасом для
summary. SQLite ограничивает историю 16 sessions / 32 MiB суммарно, вытесняя
самые старые. Ownership проверяется по workspace и существующему WebSocket
request; удаление request/workspace очищает историю, обычный SaveState её
сохраняет. Saved sessions доступны после restart через отдельные List/Get/Delete
commands и не входят в autosave snapshot запроса.

## Основа этапа 0

```text
cmd/spike ─── importer / registry / jsoncodec ─── dynamicpb / protojson
    └──────── buffered httpclient/{http10,http3}, grpcclient

spikes/desktop ─── session.Deliver ─── Sink
    └──────────── Wails Event + подтверждение доставки из UI
```

Старые bounded clients и spike CLI сохранены отдельно от streaming engine.
Их API буферизует и закрывает response до возврата из Do; это не контракт
нового HTTP execution path.

- `internal/protobuf/importer`: protocompile resolver, include paths,
  вложенные imports, WKT и проверка конфликтов.
- `internal/protobuf/registry`: реестр каждого импорта, dynamic resolver
  типов/сервисов/методов и RPC paths из descriptors.
- `internal/protobuf/jsoncodec`: lossless JSON5-like parser, ProtoJSON validation
  и dynamicpb; его normalizer используется также HTTP JSON5 body.
- `internal/transport/grpcclient`: descriptor-driven Invoke/NewStream,
  metadata/status, limits, independent send/receive и half-close.
- `internal/session`: bounded preview и batches с backpressure. Sink соблюдает
  cancellation; Deliver не создаёт фоновых goroutines.
- `cmd/spike` и `spikes/desktop`: ручные smoke/transport проверки, отдельные
  от продуктового UI. Подтверждение batch ограничивает in-flight events одним
  пакетом; отсутствие ACK за пять секунд завершает session.

В spike нет SQLite и истории, CLI явно печатает запрошенные пользователем
результаты и не является очищенным application log. Schema import и offline
Protobuf editor, unary RPC, reflection и управление gRPC streams подключены
к основному UI. WebSocket также выполняется в Go; браузер получает только
ограниченные и очищенные события и явно сохранённые sessions.
