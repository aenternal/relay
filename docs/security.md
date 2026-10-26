# Модель угроз и ограничения безопасности

Relay — локальный API-клиент с доверенным встроенным UI. Он обращается к адресам,
которые пользователь выбирает для HTTP, gRPC и WebSocket, включая localhost и
частные сети. Импорт проекта или схемы сам по себе не отправляет запросы.
Приложение не является сетевым сервисом с многопользовательской авторизацией.

## Активы и границы доверия

| Актив / граница | Доверие и защита |
| --- | --- |
| Секреты окружения | Значения находятся в OS keyring; SQLite хранит случайные references. Cleartext fallback отсутствует. Ошибки backend keyring заменяются фиксированными сообщениями. |
| Профиль, SQLite/WAL, descriptor artifacts | Пользовательские данные на локальном диске. Новые каталоги имеют mode 0700, SQLite — 0600 на Unix; на Windows действует ACL пользовательского каталога. Kernel lock исключает два desktop-процесса с одним профилем. |
| Запросы и ответы | Auth, TLS keys, payloads и headers существуют в памяти Go для исполнения. Известные секреты исключаются из превью и сохраняемой истории; редактируемые тела сохраняются только при отдельном выборе пользователя. |
| Встроенный webview → Go bridge | UI передаёт модели и opaque file grants. Физические пути не дают произвольного чтения через bridge. Команды исполнения имеют собственные ограничения и отмену. Привилегированный bridge требует доверенного UI. |
| Native picker → файловая система | Доступ возникает после выбора файла/папки. Повторное открытие проверяет regular file и identity; изменившийся выбранный файл требуется выбрать заново. Импортируемые project paths не дают файловых grants. |
| Удалённый HTTP/gRPC/WebSocket peer | Полностью недоверенные данные: TLS/host verification включены по умолчанию, размеры и retained history ограничены. HTML/JSON/Protobuf/message content показываются как текст, без выполнения scripts. |
| JSON-проект, Postman/Insomnia, `.proto`, descriptor/reflection | Недоверенный ввод с проверками размера, глубины и структуры до выделения больших деревьев. Схемы компилируются в Go без `protoc`, subprocess или remote imports. |
| Development server | Локальный вспомогательный режим с доступом к профилю и bridge. Его нельзя публиковать через внешний bind, reverse proxy, port forwarding или туннель. Он не заменяет desktop sandbox и не предоставляет HTTP-аутентификацию. |

Bundled assets имеют CSP: `default-src 'none'`, scripts/connect ограничены same
origin, запрещены objects, frames, base URL и form navigation. Asset request
logging выключен. Production tags отключают devtools и development logs.
Development server принимает только loopback bind и проверяет Host/Origin;
реальная bridge-проверка покрывает blocked inline script и 403 для чужого origin.

## Импорт, файлы и ограничение ресурсов

Project import принимает один UTF-8 JSON-документ до 32 MiB: depth 32, до 200 000
tokens, без duplicate object keys. До применения проверяются версия, ownership,
ссылки и quotas; commit атомарен и создаёт новые IDs. Scripts, hooks, tests,
flows, plugins и dynamic expressions Postman/Insomnia не выполняются. Пути
файлов, CA/client key references и прежние grants очищаются. Внешние `$ref` не
загружаются. Подробности: [формат проекта](native-project.md) и
[матрица импорта](import-compatibility.md).

Архивы ZIP/TAR, сжатые project exports, YAML и импорт схем из URL не поддержаны.
В project import нет распаковщика или автоматического определения архива по содержимому; каждый
JSON parser отклоняет ZIP с traversal entry до интерпретации его содержимого.
Отдельный [release verifier](releases.md) проверяет структуру пакетов приложения
без извлечения файлов и без исполнения их содержимого; это не импорт проекта.
`.proto` imports должны иметь нормализованные относительные виртуальные имена:
absolute paths, `..`, aliases, Windows drive/UNC paths и symlink escape из
include root отклоняются. Явно выбранный native root file может находиться вне
include directories; это отдельный пользовательский выбор. Directory scan
ограничен 8 192 entries, roots — 256, dependency closure — 512 files.

Protobuf source ограничен 2 MiB на файл и 16 MiB суммарно. Перед parser/AST
действует общий для схемы бюджет 100 000 lexical tokens/comments и 100 000
переводов строки. Punctuation внутри строк и комментариев не считается отдельным
token. Бинарные descriptors до 16 MiB проверяются до `proto.Unmarshal`: depth 100
и 100 000 structural fields/nodes; для reflection бюджет общий для всех файлов.
Offline tools допускают две параллельные операции с timeout 30 секунд.

JSON template ограничен 4 MiB, depth 64 и 100 000 structural separators/containers
до typed decoding. Ограничены количество variables и substitutions, суммарный
размер результата и обе его копии — wire и redacted. Переменные подставляются
один раз, только внутри JSON strings, с последующим escaping; object keys и
unquoted placeholders не интерполируются. Отдельный секрет не становится
исполняемым шаблоном.

Сетевые лимиты HTTP, gRPC и WebSocket описаны в [protocols](protocols.md).
gRPC использует backpressure/ACK; WebSocket удаляет старые previews со счётчиком
потерь, продолжая обработку ping/close. Отмена, закрытие вкладки и shutdown
завершают принадлежащие приложению соединения и рабочие операции. Ограничения
retained history не являются общим пределом RSS: transient wire, parser и TLS
buffers также требуют память.

## Секреты, диагностика и перенос

Ошибки OS keyring не включают строки backend, значения или references. Результат
template resolution разделяет wire value и safe preview; его JSON и стандартное
форматирование показывают только preview. Offline Protobuf validation/import не
подставляет окружение и не читает keyring. Compiler diagnostics сохраняют virtual
file/line/column и фиксированную категорию ошибки; string literals, значения
custom options и произвольные symbols из ошибки compiler не копируются в UI.
Virtual import names остаются метаданными и могут показываться для missing import.

При переносе auth literals, Cookie/Authorization, чувствительные headers/query,
URL userinfo, secret values и keyring references удаляются. Копии известных
credentials в raw, URL-escaped, base64/base64url и hex форме удаляются из остальных
переносимых текстовых полей, включая opt-in bodies. В descriptor artifact известный
credential приводит к отказу от переноса, поскольку бинарную схему нельзя безопасно
переписать как обычный текст. Export/import не читает secret values из keyring.

Тела по умолчанию не входят в перенос и persistence definitions. Для WebSocket
сохранение session payload previews имеет отдельный opt-in; metadata-only history
не содержит payload, handshake headers и close reason. Чувствительные headers и
URL query исключаются из сохранённой WS history всегда. HTTP full response save
является явным действием пользователя и может содержать чувствительные данные,
даже когда UI показывает обработанный preview.

Полный HTTP response автоматически записывается во временный файл для
ограниченного потокового чтения и последующего Save. На Unix используется mode
0600; на Windows доступ определяется унаследованным ACL временного каталога ОС,
а не Unix permission bits. Сохранённый пользователем файл также наследует ACL
выбранного каталога на Windows. Это исходное содержимое,
включая возможные секреты ответа; обработка preview его не изменяет. Forget и
штатный shutdown удаляют spool. После аварийного завершения файл может остаться
во временном каталоге ОС. Отключение сохранения тела definition не отключает
этот временный response spool.

Application diagnostics не должны включать request/response bodies, auth, Cookie,
private key bytes, URL credentials и keyring backend errors. Тестовые fixture
серверы используют синтетические canaries; проверка keyring использует внедряемый
test vault и не изменяет системные секреты пользователя. Логи приложения, OS crash
dumps и экспортированные пользователем файлы всё равно следует считать частными.

## Проверки и оставшиеся риски

| Сценарий | Проверка / защита | Остаточный риск |
| --- | --- | --- |
| Dense JSON или source AST allocation bomb | Pre-allocation structural/token/line budgets; regression с малыми числами, empty declarations/comments и общим budget нескольких files | Ограниченный крупный ввод всё ещё потребляет CPU и transient memory; лимиты не являются process sandbox. |
| Descriptor bomb, циклы и подмена import path | Wire budget до decode; offline linking; traversal/alias/symlink-negative tests | Сложные валидные схемы могут не поместиться в выбранные quotas. |
| ZIP traversal / decompression bomb | ZIP не распаковывается; все три project parser имеют negative test с `../../` entry | Добавление поддержки архивов потребует отдельного extractor с лимитами expanded bytes/files/time и containment. |
| Secret в ошибке Protobuf/keyring | Реальные compiler failures edition/custom option/symbol проверяются canary-тестом; safe backend errors | File names и schema comments являются пользовательскими данными; публичное имя само может содержать приватную информацию. |
| Копия удалённого auth в импортируемом body | Проверки raw и URL/base64/hex copies для native, Postman и Insomnia | Это не DLP: неизвестный секрет, произвольное кодирование, дробление или сжатие содержимого может сохраниться при opt-in. |
| Подмена локального профиля | Permissions, migration checksum, bounded validation, CAS и profile lock | SQLite не зашифрован и не подписан. Процесс с доступом к профилю/учётной записи пользователя не изолирован от Relay; профиль другого автора не является доверенным backup. |
| Malicious peer / сетевое перенаправление | TLS verification по умолчанию, per-request insecure opt-in, bounded previews и cancellation | Пользователь может сознательно отправить данные недоверенному endpoint, включить insecure или настроить proxy. API-клиент не ограничивает доступ к localhost/private networks. |
| Compromised webview или host account | Доверенные bundled assets, текстовое отображение peer content, opaque grants | Bridge исполняет привилегированные команды. Компрометация встроенного UI, ОС, браузерного расширения dev mode или dependency выходит за пределы secret masking. |
| Секрет в памяти / swap / crash dump | Секреты не сериализуются в штатный snapshot и не имеют plaintext fallback | Go strings, runtime, OS keyring API и TLS buffers не гарантируют zeroization; системный crash dump может содержать wire values. |
| Чувствительный HTTP response на диске | Ограниченный private response spool, удаление при Forget/штатном shutdown, явный Save исходного ответа | Spool не шифруется; crash может оставить временный файл. Disk/swap backups и OS cleanup находятся вне контроля masking UI. |
| Недоступный или зависший OS keyring | Нет fallback; context проверяется до/после вызова, ошибки безопасны | Upstream синхронный API не прерывает системный prompt, поэтому завершение использующей его операции может ждать ОС. |
| Недоверенное обновление | Автоматической загрузки/запуска update binaries нет; обновление выполняется вручную по release artifacts | Checksum без доверенного канала не доказывает авторство; подпись/нотаризация подтверждаются только для фактически подписанного выпуска. |

Статус сборок, зависимостей и платформенных smoke checks фиксируется отдельно в
[progress](progress.md). Успешная cross-compilation не подтверждает установку,
native dialogs, OS keyring или runtime на другой ОС. Windows/macOS следует считать
неподтверждёнными до проверки на соответствующем runner или реальном устройстве.
