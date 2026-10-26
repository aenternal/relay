# Возможности и ограничения протоколов

Матрица описывает реализацию этапов 0–7. HTTP, импорт схем Protobuf, локальная
проверка сообщений, gRPC unary, server reflection, streams и WebSocket подключены
к desktop UI.

| Режим HTTP | Реальный транспорт | Ограничения |
| --- | --- | --- |
| Auto | HTTP: HTTP/1.1; HTTPS: HTTP/1.1 или HTTP/2 по ALPN | HTTP/3 автоматически не пробуется; фактическая версия берётся из ответа |
| Force HTTP/1.0 | Отдельный `net.Conn` / `tls.Conn` adapter с request-line `HTTP/1.0` | Одно соединение на запрос, `Content-Length`, `Connection: close`; без chunked, CONNECT и proxy |
| Force HTTP/1.1 | `net/http` с разрешённым HTTP/1.1 | Повышение до HTTP/2 отключено |
| Force HTTP/2 | HTTPS и обязательный ALPN `h2` | При отсутствии HTTP/2 — ошибка; перехода на HTTP/1.1 нет |
| Force h2c | Штатный HTTP/2 cleartext prior knowledge | Только `http://`; h2c Upgrade и proxy не поддерживаются |
| Force HTTP/3 | `quic-go/http3`, QUIC/UDP, TLS 1.3 | Только HTTPS; без proxy и fallback на TCP |

HTTP/1.0 поддерживает потоковую передачу файлов и multipart с заранее вычисленным
размером, потоковый ответ и следование редиректам. Произвольный streaming reader
без известного размера для этого адаптера отклоняется. Современный сервер может
ответить на запрос HTTP/1.0 через HTTP/1.1: UI показывает фактическую версию ответа.
Обычные методы и корректные пользовательские method tokens поддерживаются;
ручной CONNECT запрещён во всех режимах. CONNECT внутри поддерживаемого HTTPS
proxy tunnel выполняет сам транспорт.

## Запрос и ответ

Из UI доступны URL, повторяющиеся query-параметры и headers, отключаемые строки,
экранируемые path-параметры `{id}` / `:id`, auth и настройки транспорта. Порядок
добавленных query-параметров и повторных значений сохраняется. Body: none, raw,
JSON, JSON5-like, form-urlencoded, multipart text/file и binary file. XML, HTML
и GraphQL отправляются как raw с выбранным пользователем Content-Type.

JSON5-like сначала нормализуется в строгий JSON; затем переменные подставляются
только внутрь JSON-строк. Числовые литералы, включая int64, не преобразуются
в float64. Поддерживаются комментарии, одинарные кавычки, ASCII-ключи без кавычек
и trailing commas. Hex, leading `+`, bare NaN/Infinity, multiline strings и
Unicode bare identifiers отклоняются. В raw подстановка является текстовой.
Неопределённая переменная, ошибочный URL/header или отсутствующий file grant
останавливают выполнение до подключения к серверу.

Ответ содержит status code, фактический протокол, повторные headers/trailers,
ограниченное pretty/raw/hex-превью, число полученных байтов, TLS/certificate info,
цепочку редиректов и timings. Недоступные транспорту timings остаются нулевыми;
это не обещание измерения каждой фазы для любого протокола и proxy. Hex-view
ограничен первыми 64 KiB превью. Ответы отображаются текстом, HTML не исполняется.

Сеть выполняется в Go. `StartHTTPRequest` возвращает execution ID, UI опрашивает
состояние; контекст выполнения живёт независимо от завершившегося bridge-вызова.
Общий deadline, connect/TLS/header deadlines и Cancel закрывают сетевые ресурсы.
Полученные до ошибки headers и часть тела сохраняются в результате. Ошибочный
или отменённый download может быть частичным — сохранение не восстанавливает
байты, которые клиент не получил.

## Proxy, redirects, TLS и auth

HTTP/HTTPS/SOCKS5 proxy доступны для HTTP/1.1, HTTP/2 и Auto. Проверены HTTP proxy,
HTTPS CONNECT, SOCKS5, proxy credentials и принудительный HTTP/2 через proxy.
Режим `none` отключает proxy, включая переменные окружения; `environment`
учитывает HTTP_PROXY, HTTPS_PROXY и NO_PROXY (также lowercase варианты).
ALL_PROXY этим adapter не читается. Для HTTP/1.0, h2c и HTTP/3 explicit proxy отклоняется;
в environment-режиме разрешён прямой запрос, если proxy для него не выбран.

Auth: none, Basic, Bearer, API key в header/query и inheritance. Inherit идёт
от ближайшей папки вверх к коллекции; явный none прекращает наследование.
Используется активное окружение workspace на момент запуска запроса.
Redirect policy и число переходов настраиваются. При переходе на другой origin
удаляются Authorization, Proxy-Authorization, Cookie, Referer, API key и headers /
query keys, содержащие известные секреты. Защита сохраняется и на последующих
переходах цепочки. Запрос с mTLS не следует редиректу на другой origin: адрес
нужно открыть отдельным запросом с собственным TLS profile.

Проверка сертификата включена по умолчанию, минимальная версия TLS — 1.2,
для HTTP/3 — 1.3. Доступны custom CA PEM, client certificate/private key PEM,
SNI override и явный insecure-переключатель одного запроса с предупреждением.
CA/cert/key выбираются нативным file picker; пути и байты закрытого ключа
не передаются webview и не сохраняются в определении запроса. mTLS identity
целевого сервера не передаётся HTTPS proxy.

Распаковка gzip и deflate опциональна; поддержан также raw DEFLATE, встречающийся
на некоторых серверах. Допускается не более восьми Content-Encoding layers.
Лимиты тела применяются к распакованному потоку; headers сохраняют исходный
Content-Encoding, UI предупреждает о распаковке. Сохранение такого ответа
записывает распакованные байты. Неподдерживаемую кодировку можно исследовать,
отключив распаковку.

## Ресурсы, файлы и секреты

- Общий deadline по умолчанию 30 секунд; connect/TLS — 10 секунд, headers —
  30 секунд. Эти значения меняются для каждого запроса.
- URL и request headers вместе ограничены 64 KiB; transport response headers —
  1 MiB, передаваемые UI headers/trailers дополнительно ограничены.
- Редакторский body и разрешённый результат подстановки — до 4 MiB.
  Для form/multipart действует общий лимит 4 MiB на текст и служебные поля.
  Binary/multipart files передаются потоком; upload — до 1 GiB на запрос.
- Preview по умолчанию 5 MiB; живой запрос ограничен 32 MiB. Старые настройки
  выше этого значения уменьшаются с предупреждением. Retained previews имеют
  общий бюджет 64 MiB с учётом base64; временные буферы кодирования учитываются
  отдельно от этого бюджета.
- Download по умолчанию ограничен 256 MiB на уровне application service;
  пользователь может выбрать до 1 GiB. Превышение явно помечает результат
  как частичный. Preview limit не ограничивает размер полного download.
- Одновременно выполняются до восьми запросов, хранятся до 16 результатов.
  При достижении лимита удаляется старейший завершённый результат вместе
  с временным файлом. Если бюджет занят работающими запросами, новый запуск
  отклоняется. Суммарный disk budget ограничен числом результатов и их
  индивидуальными download limits; отдельной общей disk quota нет.
- Ответ пишется в приватный временный файл с mode 0600; webview получает
  только preview. Forget, eviction и shutdown удаляют spool. По native Save
  dialog пользователь явно выбирает постоянный файл для сохранения.
- File picker выдаёт случайный opaque ID; произвольный путь из JS не даёт
  доступа к файлу. Grants живут в текущем процессе, до 256 выбранных файлов.
  При замене или изменении файла его нужно выбрать заново. TLS PEM — до 1 MiB.

История HTTP-выполнений и их ответы не записываются в SQLite. Bodies по умолчанию
остаются в памяти вкладки; сохранение raw/form/multipart текста требует opt-in.
Auth credentials и чувствительные headers/query в сохранённых определениях
должны ссылаться на environment variables; secret values находятся в OS keychain.
File grants не сохраняются. SQLite не содержит plaintext fallback для keychain.

Известные секреты маскируются в UI preview, headers, TLS metadata и diagnostics;
значения URL query маскируются полностью. Ошибки показывают безопасную категорию,
не исходный текст peer/proxy/vault. Если маскирование изменило preview, UI явно
предупреждает: сохранение пишет **исходное полученное тело**, которое может
содержать секреты. Текстовый preview с обрезанным UTF-8 не становится бинарным.
Cookie jar существует только в памяти Service; браузерные cookies не используются.

## Protobuf: импорт, инспекция и локальная проверка

Раздел Protobuf импортирует один или несколько `.proto`, папку с `.proto` либо
бинарный FileDescriptorSet. Нативный выбор поддерживает отдельные include
directories; браузерный/server mode передаёт выбранные файлы как virtual paths
и тексты. Нативные physical paths остаются в Go. Импорт работает офлайн через
`protocompile` без `protoc` и генерации клиентских stubs.

Вложенные imports и bundled WKT разрешаются в отдельном registry каждого
импорта. Одинаковые имена messages/methods в разных packages/services допустимы;
конфликтующие полные имена и virtual filenames отклоняются. Ошибки показывают
source filename, line/column и причину, включая отсутствующий import. Для
FileDescriptorSet требуется полный набор нестандартных зависимостей; пропущенные
стандартные WKT добавляются из встроенного resolver. ZIP и remote imports
не реализованы. Server reflection описан ниже.

Схемы привязаны к workspace и доступны после перезапуска без исходных файлов.
Migration v3 сохраняет metadata и self-contained descriptor artifact в SQLite
с SHA-256 checksum. Artifact исключён из обычных JSON snapshots, в webview идут
только metadata и запрошенная инспекция типов. Reimport сохраняет schema ID,
показывает предупреждения о затронутых сохранённых запросах и заменяет artifact
атомарно. Удаление схемы убирает ссылку из запросов, сохраняя method и сохранённое
с разрешения пользователя тело. Предупреждения reimport требуют повторной
проверки тела; они не заменяют полный анализ wire compatibility.

Инспектор показывает service tree, RPC kind, полные method names, input/output,
fields, enum values, oneof, maps, required/optional/presence. Выбор метода в gRPC
draft позволяет получить безопасный конечный template и проверить JSON либо
поддерживаемый JSON5-like subset. Шаблон не использует source defaults или ранее
введённые значения, оставляет maps/repeated пустыми и выбирает одну ветку oneof.

Проверка создаёт настоящее `dynamicpb` message, кодирует deterministic binary
wire, декодирует его обратно и показывает canonical ProtoJSON и base64/размер
wire. Int64/uint64 в canonical ProtoJSON имеют строковую форму; большие числовые
литералы редактора не проходят через JavaScript/Go float64. Preview — результат
локальной сериализации; RPC при этом не отправляется. Проверка не читает keyring
и не подставляет environment variables: placeholders в string fields остаются
буквальными строками. Body и результат проверки не сохраняются автоматически.

| Область Protobuf | Реализованное поведение | Ограничения |
| --- | --- | --- |
| Syntax | proto2, proto3, edition 2023 | Новые editions явно отклоняются; proto1 MessageSet не поддержан |
| Scalars | Все стандартные числовые типы, bool, string, bytes/base64, enums | ProtoJSON type/range checks; bytes требуют корректного base64 |
| Collections/presence | repeated, nested/recursive messages, maps, oneof, optional, proto2 required | Неизвестные поля и конфликтующие oneof отклоняются; required проверяются |
| WKT | Timestamp, Duration, FieldMask, wrappers, Struct/Value/ListValue, Empty | Используется официальное ProtoJSON представление каждого типа |
| Any/extensions | Dynamic resolution по импортированному registry, proto2 extensions | Нестандартный тип должен присутствовать в registry; неизвестный type URL — ошибка |
| Diagnostics | Line/column/path для JSON/ProtoJSON, безопасная причина | Значения тела и неизвестные keys/type URLs в error text не раскрываются |

JSON5-like subset совпадает с HTTP normalizer: comments, одинарные кавычки,
ASCII bare keys и trailing commas. Hex numbers, leading `+`, bare NaN/Infinity,
multiline strings и Unicode bare identifiers отклоняются. Значения `NaN`/
`Infinity`, разрешённые ProtoJSON для float/double как **строки**, обрабатываются
по правилам ProtoJSON. Некорректное тело не создаёт wire preview.

Лимиты импорта: 512 dependency files, 256 roots, 64 include directories,
8192 entries при сканировании папки, 2 MiB на source, 16 MiB суммарных sources
или descriptor set. Сохраняются до 64 схем; общий snapshot ограничен 32 MiB,
включая base64 artifacts и остальные данные. Native picker принимает до 32
дополнительных include directories, Go compiler — до 64 итоговых include roots.
Список root names descriptor set ограничен 256, полный artifact сохраняется.
Input и wire ограничены 4 MiB, recursion — 100; canonical JSON — 8 MiB с compact
fallback для большого pretty output. Template ограничен 256 KiB, depth 12 и
2048 fields. Две операции импорта/инспекции/валидации могут работать одновременно;
при занятом бюджете возвращается busy error. Deadline — 30 секунд; shutdown
отменяет active work. Это ограничения редактора, отдельно от transport
лимитов gRPC Go-core.

## gRPC unary и server reflection

В gRPC-вкладке доступны endpoint `host:port`, выбранная схема и полный method
path `/package.Service/Method`, TLS/plaintext, custom CA/mTLS, server name,
authority, повторяющиеся metadata, connect timeout и общий deadline. TLS проверяет
сертификат по умолчанию; insecure требует явного включения. CA, certificate
и private key выбираются через native file picker: пути и key bytes остаются
в Go, grants не переживают перезапуск. Plaintext сохраняет TLS profile как
неактивный, показывает предупреждение и не читает его файлы. gRPC adapter
подключается напрямую и не использует HTTP_PROXY/HTTPS_PROXY.

Metadata names должны быть lowercase; transport-reserved names отклоняются.
`-bin` values вводятся как standard base64 и преобразуются в raw bytes для grpc-go;
ответные binary metadata снова показываются как base64. Доступна request
compression none/gzip. Auth передаётся metadata, например
`authorization: Bearer {{token}}`, с переменными активного окружения workspace.
Чувствительные literal metadata остаются только в текущем draft; для сохранения
используются environment references и OS keyring.

Send проверяет JSON/JSON5-like по выбранному input descriptor и передаёт настоящее
binary Protobuf через grpc-go `Invoke` по HTTP/2. Переменные подставляются только
в JSON strings после normalization; int64 сохраняется без float64. Это отличается
от локальной кнопки проверки, которая не читает окружение/keyring и не отправляет
RPC. Для unary выбранный streaming method отклоняется с пояснением; server,
client и bidi методы используют отдельный stream lifecycle ниже.

Ответ показывает canonical ProtoJSON, hex исходных Protobuf bytes, размер,
initial headers, trailers, status code/message/details, TLS и connect/total timing.
Raw binary копируется из входного buffer codec после gRPC decompression и сохраняет
порядок полей и duplicate singular tags; gRPC framing в это представление не входит.
Известные status detail types декодируются по локальному registry, стандартные
google.rpc types имеют встроенный fallback. Неизвестный Any сохраняет type URL
и безопасные raw bytes с пояснением. При non-OK status headers/trailers/details
остаются доступны. Ответ и execution state не сохраняются в SQLite.

Cancel и deadline завершают сетевой вызов; завершение bridge invocation не
отменяет worker. ОС keyring использует синхронный upstream API: Start/Cancel
остаются отзывчивыми, но системный prompt не прерывается context. Worker
и shutdown могут ждать, пока системный вызов вернётся. Известные secrets
маскируются в JSON/metadata/status/TLS/error. Raw binary скрывается при обнаружении
секрета или redaction JSON; повреждённый/слишком большой после redaction JSON
не выдаётся как частично корректный документ.

Discover via Reflection запускается явно: v1, затем v1alpha только при
Unimplemented. Reflection использует тот же TLS/authority/metadata profile;
неполная или конфликтующая dependency closure отклоняется. Результат сохраняется
как self-contained descriptor schema с checksum. Повторный discovery того же
connection profile в текущем процессе обновляет прежний schema ID. Session cache
fingerprints не сохраняется; сам artifact доступен офлайн после перезапуска.
Unary не выполняет reflection автоматически. Если сервер его отключил,
пользователь получает подсказку импортировать `.proto` или descriptor set;
вызов по локальной схеме продолжает работать.

Лимиты gRPC application: 30 секунд общего deadline и 10 секунд connect по
умолчанию; send/receive — 4 MiB по умолчанию и не более 4 MiB, metadata — 64 KiB.
До четырёх активных операций, 16 сохранённых в памяти результатов и 64 MiB общего
Go result budget с резервированием места до запуска. JSON — до 8 MiB, details —
до 128 elements и 1 MiB в сумме, protobuf recursion — 100. Старейший завершённый
result без ожидающих ACK stream messages вытесняется при нехватке места;
временные schema/codec buffers отдельно
от retained budget. UI удерживает до 16 завершённых results и 32 MiB строк,
не вытесняя активные операции. Reflection ограничен 512 файлами, 16 MiB
descriptors, 2048 services/requests, deadline 30 секунд; live profile cache —
256 fingerprints. Persisted schemas используют общие ограничения snapshot выше.
Перед разбором binary descriptors действует общий структурный бюджет 100 000
единиц работы и глубина 100; в reflection бюджет суммируется по полученным файлам,
включая повторы. Это ограничивает выделения памяти до protobuf unmarshal.

## gRPC server/client/bidi streams

Тип RPC берётся из descriptor. Server stream автоматически отправляет одно
текущее body и закрывает направление отправки. Client stream и bidi открываются
без автоматической отправки; Send передаёт очередное тело, Half-close завершает
только отправку. Receive остаётся активным до EOF/ошибки. Bidi допускает следующий
Send после уже полученного ответа на том же RPC. Zero-message client/bidi stream
можно завершить Half-close. Для client-streaming сервер возвращает один ответ.

JSON/JSON5-like нормализуется и проверяется отдельно для каждого Send; невалидное
тело не закрывает stream и не отправляется в сеть. Endpoint, schema, TLS, metadata
и environment фиксируются при подготовке stream. Изменение editor settings
влияет на следующий запуск. Keyring читается только в Go при первом использовании
ссылки; значения кэшируются в пределах stream, суммарно до 4 MiB raw secrets.
Новые известные секреты маскируют следующие сообщения, status/details/errors,
headers и ещё не подтверждённые previews. Маскирование скрывает raw binary;
уже ACK-нутая UI история хранится отдельно.

Лента показывает sent/received, отдельный index каждого направления, общий sequence,
timestamp, JSON/hex, размер и предупреждения. Incoming hex — точный decompressed
Protobuf payload с сохранённым порядком полей; outgoing hex — сериализация
отправленного message, порядок map-полей может отличаться от grpc-go marshal.
При ошибке доступны уже принятые сообщения, headers и полученные final
status/details/trailers. Cancel и общий deadline останавливают оба направления.
Закрытие ограниченно дочитывает buffered DATA для доступного final status;
отброшенные при этом previews отмечаются предупреждением. Если final status
не удалось получить, результат использует code отмены/deadline либо ошибки.
Half-close не заменяет Cancel и не отменяет receive. Без Half-close client/bidi
может оставаться открытым до deadline или завершения сервером.

Очередь Go ограничена 64 сообщениями и 14 MiB previews на stream; ещё 2 MiB
резерва покрывают status/metadata внутри 16 MiB reservation общего gRPC budget.
Poll выдаёт до 32 сообщений и примерно 1 MiB preview strings; одно большее
сообщение выдаётся отдельно. UI подтверждает batch явным ACK после принятия
в локальную историю. ACK освобождает Go queue, повторный ACK безопасен.
При отсутствии ACK producer ждёт, сохраняя backpressure; Cancel/deadline
прерывают ожидание. После terminal очередь остаётся доступна для drain.
Новый запуск не вытесняет terminal stream с pending messages, поэтому при
занятом бюджете сначала нужно дочитать/очистить результат. Send и Half-close
не ставятся в очередь bridge-команд: параллельная команда получает busy error.
Команды до readiness и после закрытия отклоняются с понятной причиной.

UI хранит ограниченную историю отдельно от Go queue и отрисовывает её окном.
Общий бюджет строк 32 MiB делится между unary/reflection results и stream history;
при достижении лимита старые сообщения удаляются с видимым предупреждением.
Stream messages и результаты не записываются в SQLite.

## WebSocket

ws:// и wss:// используют HTTP/1.1 Upgrade через закреплённый `coder/websocket`.
Доступны URL, повторные query/headers, Origin/Cookie и список subprotocols.
Handshake diagnostics показывают HTTP status, ответные headers, выбранный
subprotocol, connect time и реальный TLS state. Redirect не выполняется;
на серверный отказ сохраняются status/headers без исходного error-page body.
Cookie jar HTTP не переносится автоматически: Cookie задаётся в handshake.
Header Upgrade/Connection/Sec-WebSocket-* управляется transport, WebSocket
compression и HTTP/2 Extended CONNECT не реализованы.

TLS verification включена по умолчанию; custom CA, SNI, mTLS и отдельный insecure
opt-in используют нативные opaque grants. Неактивный TLS profile на ws://
игнорируется. Proxy none/environment/custom поддерживает HTTP/HTTPS/SOCKS5;
target mTLS identity не передаётся HTTPS proxy. Для environment proxy учитываются
HTTP_PROXY/HTTPS_PROXY/NO_PROXY, ALL_PROXY не читается. Subprotocol выбирает сервер
из предложенных вариантов; response text/HTML всегда отображается безопасным
текстом.

Connect возвращает ID сразу и работает на context приложения. Text Send требует
корректную UTF-8; binary Send принимает hex, standard base64 либо файл
из нативного picker. Неактивные body/file поля не отправляются. Один успешный
Send создаёт одно логическое сообщение, fragmentation прозрачен для приложения.
Hex принимает ASCII space/tab/CR/LF между байтами, остальные не-hex символы
отклоняются. Credential masking учитывает каждый Cookie value отдельно,
поэтому echo одного токена без `session=` также скрывается.
Окружение фиксируется при подключении, дальнейшие templates разрешаются в Go
с тем же snapshot. Secret values читаются из keyring при первом использовании
и кэшируются до закрытия; raw keyring cache ограничен 256 KiB, environment —
1024 variables / 1 MiB. Новые известные секреты очищают pending events и history
ring; текст, уже показанный до первого использования секрета, не отзывается из UI.

Defaults: connect/write 10 секунд, read timeout выключен, message 1 MiB.
Лимит сообщения можно поднять до 4 MiB; read timeout ограничивает ожидание
следующего полного data message. Текстовый template/resolved value — до 4 MiB.
Для literal binary без `{{references}}` принимается до 8 MiB hex или эквивалентный
base64, чтобы получить 4 MiB wire data. Binary с references проходит общий
4 MiB template limit; файл также должен помещаться в выбранный message limit.
Preview текста и raw binary ограничен первыми 64 KiB, полный размер сохраняется.
Known secret маскируется в text и diagnostics; binary preview с ним скрывается.

Reader независим от Send и продолжает обслуживать ping/pong/close при медленном
UI. Send/Ping не ставятся в очередь: параллельная команда получает busy error,
ручной Ping ограничен одним в секунду. Autopong включён; payload control frames
не выводится, события показывают направление и размер. Disconnect выполняет
graceful close с timeout 1 секунда, Cancel прерывает physical connection.
CleanClose означает реальный peer close; 1008 policy close тоже может быть
чистым протокольным завершением. Потеря TCP получает 1006, локальное превышение
receive limit — 1009, invalid UTF-8 — 1007, с `CleanClose=false`.
Shutdown отправляет 1001, ждёт ограниченный graceful close и закрывает ресурсы.
Системный keyring prompt, как и для gRPC, не прерывается Go context.

Go держит до четырёх active connections, 16 transient records и общий retained
budget 64 MiB. Session резервирует 16 MiB: 64 events / 8 MiB UI queue, отдельный
history ring 2000 events / 6 MiB и 2 MiB overhead. Временные message/encoding
buffers учитываются отдельно. Полная queue вытесняет старые events с видимым
dropped count; ожидание ACK не приостанавливает network reader. Control callback
при занятом mutex пропускает notification и увеличивает тот же счётчик.
Poll возвращает до 32 events / примерно 1 MiB strings, ACK освобождает доставленный
prefix. После terminal возможен drain, pending terminal queue не вытесняется
новым запуском автоматически. Forget явно удаляет transient session.

Save session доступен после закрытия. По умолчанию сохраняются metadata,
счётчики и event sizes без text/base64, close reason и handshake headers.
Отдельный opt-in включает только очищенные ограниченные previews; это не архив
исходных payloads. Перед Save история заново проверяется по всем известным
секретам; URL query values не сохраняются даже в opt-in history. History ring
и дополнительное усечение для JSON escaping отмечаются omitted count.
SQLite хранит до 16 sessions / 32 MiB суммарно, одна запись вместе с summary —
до 8 MiB; самые старые вытесняются. History доступна после restart, чтение/удаление
ограничено workspace/request ownership. Autosave definitions не сохраняет session
автоматически; удаление request/workspace удаляет связанную историю.

## Остальные протоколы и платформы

Перенос definitions не меняет транспортные правила. Native `relay-project/v1`
сохраняет HTTP, gRPC и WebSocket options и self-contained descriptor artifacts,
чтобы после импорта можно было вызвать запрос с локальной схемой без reflection.
Тела требуют отдельного включения; credentials, keyring references и file grants
не переносятся. Секреты вводятся заново, PEM и payload files выбираются повторно.
Импорт сам не открывает сетевое соединение. Postman v2.1 и Insomnia JSON v4
создают HTTP definitions только в документированном подмножестве; scripts,
tests, flows, remote references и template plugins не исполняются. См.
[native format](native-project.md) и [import compatibility](import-compatibility.md).

| Возможность | Проверено | Оставшиеся ограничения |
| --- | --- | --- |
| Protobuf UI/core | Source/folder/include import, binary descriptor sets, SQLite restart, service/method/message inspector, templates | ZIP и remote imports не реализованы |
| ProtoJSON | Dynamic wire roundtrip, scalar/WKT matrix, exact int64, nested/repeated/map, oneof, Any, proto2 extensions | Локальная проверка без environment/keyring; Send отдельно подставляет окружение |
| gRPC UI/core | Unary и server/client/bidi streams с локальной/reflected схемой, v1/v1alpha reflection, metadata, headers/trailers, status/details, Half-close/Cancel, bounded history/ACK | Вызовы используют grpc-go HTTP/2; stream messages не сохраняются в SQLite |
| gRPC TLS | CA, custom CA, SNI/authority, insecure, mTLS | Нативные PEM grants нужно выбрать снова после перезапуска |
| Desktop UI | React/TS, CodeMirror, autosave, SQLite restart, HTTP/gRPC/WebSocket lifecycle, Protobuf import/editor/reflection, timelines и явная WebSocket history | HTTP/gRPC responses не сохраняются в SQLite |
| Native Linux | Реальное GTK окно: GTK 3.24.52, WebKitGTK 2.52.6; project export/import через настоящие OS Save/Open dialogs, preview и новое workspace | Этот запуск не подтверждает Windows/macOS runtime или все системные keychain |
| Windows/macOS | Отдельные сборочные/платформенные проверки перечислены в progress | Native runtime и реальные keychain на этих ОС пока не подтверждены |
| WebSocket | ws/wss, custom CA/mTLS, text/binary/file, headers/query/subprotocols, Ping/Disconnect/Cancel, bounded events и явная session history | HTTP/1.1 Upgrade; без permessage-deflate, Extended CONNECT и raw payload archive |
| Project transfer | Native v1 с HTTP/gRPC/WebSocket и descriptors; Postman Collection v2.1, Insomnia JSON v4 с preview/report | Новое workspace; тела opt-in, секреты/файлы вводятся заново; OpenAPI import и OAuth2 helpers не реализованы |

В gRPC один Send и один Recv могут работать независимо. CloseSend
закрывает только направление к серверу; final status/trailers получают чтением
до EOF/ошибки. Брошенный stream отменяют через Cancel. Пути RPC формируются
из descriptors: `/relay.test.v1.EchoService/Unary`.

Onyx healthcheck вручную добавляет gRPC framing поверх HTTP/3. Relay не заявляет
поддержку такого нестандартного серверного режима: обычный gRPC использует
`grpc-go` поверх HTTP/2, HTTP/3 является отдельным HTTP transport.
