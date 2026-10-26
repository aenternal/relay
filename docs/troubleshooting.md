# Решение проблем

Версия, commit, платформа, Go toolchain и статус подписи видны в настройках Relay.
Их можно получить без открытия GUI и профиля командой `relay --version`
(`relay.exe --version` на Windows). Для сообщения о проблеме достаточно этих
метаданных и безопасного текста ошибки. Не прикладывайте профиль SQLite/WAL,
полные ответы, exports с bodies, файлы ключей или значения секретов.

## Приложение не запускается

На Linux desktop-сборке нужны GTK3 и WebKitGTK 4.1. Portable архив не включает
системные библиотеки. Debian/Ubuntu package объявляет зависимости `libgtk-3-0`
и `libwebkit2gtk-4.1-0`; на системах с t64 используются совместимые replacements.
Если пакетный менеджер не может подобрать зависимости, используйте совместимую
версию дистрибутива или соберите приложение с его native development packages.
Для локальной сборки нужны Go 1.26.10, Node 24.19.0, GTK3/WebKitGTK development
packages и предварительно собранный `frontend/dist`.

Linux native запускается в пользовательской графической сессии с доступным
display. Запуск через SSH или без session bus может не открыть окно или keyring.
Server mode предназначен для локальной разработки и тестирования; он не заменяет
установку desktop-приложения и не предоставляет native file dialogs.

На Windows требуется Microsoft WebView2 Runtime. Текущая поставка — portable ZIP
без installer и Authenticode signature. Windows GUI/runtime и системные dialogs
ещё не подтверждены запуском в текущем Linux окружении. macOS `.app` создаётся
только на native runner; DMG, signing и notarization не выполнены. Неподписанную
сборку может заблокировать Gatekeeper. Статус **UNSIGNED** не следует считать
ошибкой проверки checksum или доказательством происхождения пакета.

Для проверки скачанного каталога пакетов используйте команды из
[releases](releases.md). Проверяющий инструмент не исполняет содержимое архива.
Совпадение SHA-256 подтверждает целостность относительно manifest; доверие к
автору требует доверенного канала получения manifest или проверенной подписи.

## Профиль уже открыт или не сохраняется

По умолчанию данные находятся в `os.UserConfigDir()/relay`, файл — `relay.db`.
`RELAY_DATA_DIR` выбирает другой каталог. Одновременно пользоваться одним
профилем может один desktop-процесс. Закройте прежнее окно или завершите
принадлежащий вам процесс; kernel lock освобождается после его выхода.
Удалять lock-файл или SQLite/WAL во время работы приложения не требуется.

Проверьте доступ к каталогу, свободное место и выбранный `RELAY_DATA_DIR`.
Не открывайте профиль на сетевом диске одновременно из разных процессов.
Для диагностики можно запустить Relay с отдельным пустым каталогом профиля;
существующие данные при этом не затрагиваются. Проект другого автора лучше
переносить через JSON import с preview, а не подменой всей SQLite.

При ошибке миграции или неизвестной версии сохраните резервную копию закрытого
профиля. Не исправляйте migration ledger и checksum вручную. Конфликт revision
означает, что данные изменились после загрузки: перечитайте workspace и повторите
изменение. Импорт нового workspace не должен заменять существующие коллекции.

## Keychain недоступен

Linux использует Secret Service в пользовательской D-Bus session, macOS —
Keychain, Windows — Credential Manager. Убедитесь, что системное хранилище
разблокировано и доступно текущему пользователю. Relay не сохраняет секрет
в SQLite или plaintext-файл при отказе keyring. Обычные public variables и
workspace при этом доступны.

Секрет не возвращается из keyring в форму. Чтобы изменить его, введите новое
значение и сохраните. После импорта проекта secret declarations пусты: значения
нужно ввести заново. Выберите нужное окружение workspace и убедитесь, что ссылка
`{{token}}` соответствует существующей secret variable.

Системный prompt может ждать ответа пользователя. Upstream keyring API нельзя
прервать Go context, поэтому уже начавшийся вызов и shutdown могут ждать ОС.
Не повторяйте многократно сохранение секрета, пока системный диалог открыт.

## TLS, CA, SNI и mTLS

Проверьте схему URL и режим TLS/plaintext. gRPC принимает endpoint `host:port`,
а не HTTP URL. Для private/self-signed CA выберите доверенный CA PEM через
native picker. Server name/SNI должен соответствовать имени сертификата;
подключение по IP к сертификату для DNS-имени обычно требует корректного SNI.
Для mTLS выберите client certificate и соответствующий private key.

После перезапуска и после импорта TLS-файлы нужно выбрать заново: grants
действуют только в текущем процессе. Если файл изменился после выбора, приложение
попросит повторить выбор. Пароль от зашифрованного PEM автоматически не запрашивается;
используйте поддерживаемую пару PEM с подходящими правами доступа.

Insecure явно отключает проверку сертификата для конкретного запроса и показывает
предупреждение. Для постоянной настройки предпочтительно указать корректный CA
и server name. Insecure не исправляет неверный endpoint, отсутствующий mTLS
certificate, несовместимый протокол или сетевой timeout.

## Proxy, HTTP/2 и HTTP/3

Proxy по умолчанию выключен. Для environment proxy выберите этот режим явно;
для custom proxy проверьте URL, схему и доступность. Не переносите proxy password
в публичный URL или export: используйте переменные окружения. HTTP/HTTPS/SOCKS5
поддерживаются в документированных сочетаниях transport; HTTP/3 с explicit proxy
не поддержан. Матрица находится в [protocols](protocols.md).

Forced HTTP/2 требует TLS с ALPN `h2`; h2c предназначен для сервера с plaintext
HTTP/2 prior knowledge. Forced HTTP/3 требует QUIC/UDP, соответствующий порт и
TLS-сертификат. Проверьте firewall, VPN и доступность UDP у сервера. Forced
version не выполняет скрытый fallback: отсутствие H3 даёт ошибку. Чтобы проверить
обычный endpoint, выберите Auto либо HTTP/1.1 и сравните фактический протокол.

## gRPC и reflection

Для `Unavailable` проверьте endpoint, порт и TLS/plaintext, затем proxy/firewall.
Для `Unauthenticated` проверьте активное окружение и metadata, например
`authorization: Bearer {{token}}`. Имена binary metadata заканчиваются на `-bin`,
их значения задаются base64. Большие вызовы могут требовать изменения deadline
и send/receive limits; разрешённые максимумы остаются ограничены.

Discovery сначала использует reflection v1, затем v1alpha только при
`Unimplemented`. Сервер может полностью отключить reflection. Импортируйте
локальные `.proto` с зависимостями или полный `FileDescriptorSet`, выберите схему
и метод и отправьте RPC без reflection. Ошибка permissions, TLS или authentication
не исправляется переключением версии reflection.

Server-streaming начинает отправку кнопкой Start stream; client-streaming и bidi
открываются без первого сообщения. Send message отправляет текущий editor body,
Half-close завершает отправку и оставляет приём открытым. Cancel отменяет оба
направления. Полная UI queue может приостановить gRPC receive до ACK; это
backpressure, а не архив бесконечного числа сообщений.

## WebSocket

Используйте `ws://` или `wss://` endpoint с HTTP/1.1 Upgrade. Сервер должен
принимать заданные Origin, auth headers и subprotocols. HTTP 401/403 или отказ
в Upgrade проверяйте в Handshake. WebSocket поверх HTTP/2/HTTP/3 и автоматический
reconnect/heartbeat не реализованы. Ping доступен раз в секунду.

Binary editor принимает hex, base64 или явно выбранный файл; hex допускает
пробелы и переводы строк. Лимит logical message по умолчанию 1 MiB, максимум
4 MiB. Сообщение может состоять из нескольких frames, но UI показывает logical
message. При большом потоке ранние previews удаляются со счётчиком пропусков;
ping/close продолжают обрабатываться.

Disconnect выполняет close handshake, Abort прерывает соединение. Код 1006
обычно означает отсутствие подтверждённого close frame. Сохранение сессии
доступно после завершения; по умолчанию сохраняется metadata без payload.
Опция payloads сохраняет ограниченные обработанные previews, а не полные
исходные сообщения. История ограничена 16 sessions / 32 MiB.

## Protobuf и импорт проектов

Для source import выберите roots и include directories так, чтобы виртуальные
`import` paths разрешались относительно include roots. Missing import diagnostic
показывает имя зависимости и позицию; добавьте файл или include directory.
Remote imports, ZIP/TAR и Edition 2024 не поддержаны. Descriptor set должен
включать зависимости; bundled WKT разрешаются офлайн. Compiler diagnostic
скрывает значения literals и произвольных symbols, сохраняя место ошибки.

Validate работает офлайн без подстановки окружения. В JSON значения templates
нужно заключать в строки: `{"token":"{{token}}"}`. Bare `{{count}}` и placeholders
в object keys не интерполируются. Для int64/uint64 можно использовать строки,
например `"9223372036854775807"`. Dense/deep JSON, слишком большие sources и
descriptor graphs отклоняются по ограничениям памяти.

Проект — UTF-8 JSON до 32 MiB; поддержаны `relay-project/v1`, Postman Collection
v2.1 и Insomnia JSON v4. YAML v5, другие версии и архивы отклоняются. Для Postman
и Insomnia переносится документированное HTTP-подмножество; scripts/tests/flows
не выполняются. Preview показывает пропуски и ничего не меняет в профиле.

Bodies по умолчанию исключены; их включение выбирается отдельно при экспорте
и импорте. Это не разрешает перенос известных credentials. Известные секреты и
их обычные URL/base64/hex copies очищаются, file grants и keyring references
исключаются всегда. Неизвестный секрет внутри произвольного body всё же может
попасть в файл при явном включении bodies; проверьте содержимое перед передачей.
Подробнее: [native project](native-project.md),
[import compatibility](import-compatibility.md), [security](security.md).

## Локальный server mode и отчёт об ошибке

Server mode разрешает loopback bind, например `127.0.0.1`; адрес `0.0.0.0`
отклоняется. Открывайте выданный локальный адрес напрямую. Запросы с посторонним
Origin/Host могут получить 403: публичный reverse proxy, domain rebinding и
туннели к bridge не поддерживаются. Не включайте внешний bind ради обхода ошибки.

Если проблема повторяется, укажите версию/commit/platform из настроек или
`--version`, протокол, действие и безопасный текст ошибки. Для сетевого сбоя
достаточно схемы соединения, состояния TLS/proxy и согласованного протокола;
значения auth, Cookie, URL query и payloads не нужны. Для воспроизводимости
предпочтителен небольшой fixture с вымышленными данными.
