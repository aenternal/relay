# ADR 002: SQLite, профиль приложения и системное хранилище секретов

Дата: 2026-10-23. Статус: принято для этапа 1.

По указанию пользователя реализация этапа 1 продолжена при открытой проверке
нативных webview из этапа 0. Это не подтверждает запуск Linux/macOS desktop:
браузерная проверка использует настоящий Wails bridge в server mode.

## Сохранение данных

`modernc.org/sqlite` **v1.44.3** — pure Go драйвер без обязательного CGO, выбранный
после проверки закреплённого API/Go module. `database/sql`, WAL, foreign keys,
busy timeout, одна connection в пуле на экземпляр репозитория. Миграции SQL
встроены в binary, выполняются транзакционно, имеют последовательные версии и
checksum; неизвестная или изменённая схема отклоняется.

Таблицы `workspaces`, `collections`, `folders`, `requests`, `environments`,
`variables`, `settings`, `ui_state`, `state_metadata`, `schema_migrations` и
`secret_cleanup` содержат нормализованные связи и JSON payload моделей. Снимок
записывается одной транзакцией с optimistic revision. Внутри application service
команды сериализуются. Это достаточно для локальных коллекций этапа 1;
частичные обновления больших workspace можно добавить без изменения API UI.

Новые каталоги профиля получают mode 0700, файл SQLite — 0600 на Unix. Windows
использует пользовательский каталог и его ACL; Unix mode не заменяет Windows
access control. Desktop до открытия БД удерживает kernel file lock профиля
(flock / LockFileEx), освобождаемый ОС при падении процесса. Одновременная работа
двух desktop-процессов с одним профилем запрещена: это также защищает операции
между SQLite и внешним keychain.

## Секреты

`github.com/zalando/go-keyring` **v0.2.6**: Secret Service на Linux, Keychain на
macOS, Credential Manager на Windows. В БД хранится случайный `SecretRef`, а
значение передаётся в vault непосредственно. Значения не возвращаются через
`LoadState` и не включаются в application-логи. При недоступном keychain сохранять
секрет нельзя; plaintext fallback отсутствует. Публичные переменные и коллекции
при этом работают.

Замена секрета выполняется copy-on-write: новая ссылка сначала записывается в
очередь очистки, затем значение — в vault. Транзакция SQLite атомарно делает
новую ссылку активной, убирает её из очереди и ставит прежнюю ссылку на удаление.
Удаление окружения/переменной сначала фиксируется в SQLite. Очистка vault
производится после commit; неудачные операции остаются в durable outbox и
повторяются при старте и следующих изменениях. В outbox только ссылки, без
значений. Это позволяет пережить отказ БД, заблокированный keychain или падение
между двумя хранилищами без повреждения прежнего активного секрета.

Upstream keyring API синхронен и не поддерживает прерывание системного prompt.
Context проверяется до/после вызова, дополнительные зависающие goroutines не
создаются. Проверка доступности не держит application mutex, а UI загружает её
независимо от workspace; системный prompt не блокирует обычную работу с БД.

## Ограничение чувствительных данных

- Тело запроса не передаётся в persistence bridge и не пишется в БД, пока
  пользователь не включил сохранение тела. Память вкладки хранит transient body;
  закрытие предлагает сохранить с явным opt-in, отменить либо удалить черновик.
- Authorization/Cookie/API key и чувствительные query values требуют ссылок
  `{{variable}}`; literal credentials в URL userinfo отклоняются.
- Секреты environment редактируются password input и очищаются из form state
  после передачи. Для очистки значения весь snapshot окружения с пустым ref
  сохраняется атомарно, без преждевременного удаления keychain item.
- Подстановка в JSON работает только внутри строк и экранирует значения;
  `json.Number` сохраняет точность чисел. Голые placeholders и подстановка в
  object keys отклоняются. Нет рекурсивной интерполяции.

Ссылки на API:
[SQLite driver](https://pkg.go.dev/modernc.org/sqlite),
[go-keyring v0.2.6](https://github.com/zalando/go-keyring/tree/v0.2.6),
[Wails bindings](https://v3.wails.io/features/bindings/methods/).
