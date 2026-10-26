# ADR 001: отдельное Go-ядро и Wails v3 spike

Дата: 2026-10-23. Статус: принято; native Linux smoke пройден после установки GTK.

## Решение и проверенные версии

| Компонент | Закреплённая версия | Проверка |
| --- | --- | --- |
| Go | 1.26.8 | Локальный Linux amd64 toolchain |
| quic-go | 0.64.1 | Требует Go 1.26; локальный HTTP/3 TLS/UDP, cancel и race |
| grpc-go | 1.84.0 | Unary и streaming через runtime descriptors, TLS/mTLS, race |
| protobuf | 1.36.11 | dynamicpb, protojson resolver, Any, exact int64 |
| protocompile | 0.14.1 | Go 1.21 minimum; imports, WKT, offline runtime compilation |
| Wails | 3.0.0-beta.34 | Native Linux GTK3/WebKitGTK, server bridge, Windows amd64 cross-build |
| Playwright | 1.56.1 | Headless Chromium bridge smoke; package-lock.json |

API сверены с исходниками закреплённых модулей и официальными источниками:
[quic-go client](https://quic-go.net/docs/http3/client/),
[grpc-go](https://pkg.go.dev/google.golang.org/grpc@v1.84.0),
[protocompile 0.14.1](https://pkg.go.dev/github.com/bufbuild/protocompile@v0.14.1),
[ProtoJSON](https://protobuf.dev/programming-guides/json/),
[Wails beta.34 release](https://github.com/wailsapp/wails/releases/tag/v3.0.0-beta.34),
[Wails events](https://v3.wails.io/guides/events-reference/).

Go core не зависит от Wails и может тестироваться без webview. Native shell
изолирован в `spikes/desktop` отдельным модулем: его platform dependencies не
влияют на `go test ./...` основного модуля. Весь HTTP/gRPC транспорт выполняется
в Go. HTML/JS в spike предназначен только для проверки bridge; React/TypeScript,
Vite, SQLite и основная структура UI относятся к этапу 1.

HTTP/1.0 реализован отдельным TCP/TLS writer: выставление `Request.Proto` у
`net/http.Client` не доказывает wire version. HTTP/3 использует только
`http3.Transport`, без автоматического fallback. Повторяющиеся заголовки и
лимиты проверяются до отправки; response целиком буферизуется в пределах лимита.

Protobuf компилируется без `protoc`, реестр изолирован на импорт; Any использует
тот же resolver. JSON5-like написан как небольшой ограниченный нормализатор:
числовые лексемы не превращаются в float64, поэтому MaxInt64 не округляется.
Строгую семантическую проверку выполняет официальный `protojson`.

gRPC реализован через `ClientConn.Invoke` и `NewStream`, включая все streaming
flags. Код не копирует ручной gRPC framing из healthcheck-примера: этот Onyx
режим поверх H3 должен быть отдельной совместимостью с собственными тестами.

## Ограничения shell и gate

После установки пользователем GTK3/WebKitGTK 4.1 development packages проверены
нативная сборка `go build -tags gtk3` и настоящее окно основного приложения:
GTK 3.24.52, WebKitGTK 2.52.6, Linux amd64. Окно загрузило React assets и начальный
workspace через Go bridge. Headless server mode с `CGO_ENABLED=0`
собирается и проходит настоящий браузерный тест: 10 000 ordered events порциями,
ACK, отмена долгого задания, повторный запуск. Контроллер отдельно проверен race.
Windows amd64 native binary кросс-компилируется; исполнение на Windows/macOS
требует соответствующих сред. Подготовлены CI jobs, но
удалённый CI в этой сессии не запускался.

Оснований переходить на Wails v2 не обнаружено. Linux native gate закрыт;
проверка webview и системных интеграций остальных ОС остаётся частью release
validation. Shell beta остаётся отдельным риском без зависимости Go-core от Wails.

На локальной Ubuntu 26.04 Playwright 1.56.1 не распознал platform; браузерный
smoke выполнен с `PLAYWRIGHT_HOST_PLATFORM_OVERRIDE=ubuntu24.04-x64` и
`playwright install chromium --only-shell`. CI использует поддержанную Ubuntu
24.04. GTK и системные библиотеки установлены пользователем.
