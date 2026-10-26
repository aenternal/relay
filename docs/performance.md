# Производительность и завершение процесса

Ограничения ниже описывают сохраняемые приложением previews и очереди.
Это не предел RSS процесса: Go heap, WebKit, protobuf descriptors, временные
codec/JSON/base64 buffers, исходные сообщения, keyring snapshots и копии bridge
учитываются отдельно. Квоты разных протоколов также независимы.

## Ресурсные границы

| Ресурс | Граница и поведение при заполнении |
| --- | --- |
| HTTP в Go | 8 активных запросов, 16 результатов, 64 MiB preview reservation с учётом base64. Завершённые результаты вытесняются первыми. |
| HTTP download | По умолчанию 256 MiB на ответ, максимум 1 GiB. Ответ пишется в private spool, preview не требует загрузки полного ответа в память. Сохраняется не более 16 spools, то есть до 4 GiB при defaults или до 16 GiB при максимальном download limit. |
| gRPC в Go | 4 активные операции, 16 результатов, 64 MiB retained result budget. Stream заранее резервирует 16 MiB, включая очередь до 64 сообщений / 14 MiB и 2 MiB для status/metadata. Полная очередь ждёт ACK или cancellation. |
| WebSocket в Go | 4 активных соединения, 16 записей, 64 MiB reservation. На активное соединение: очередь до 64 событий / 8 MiB, отдельная история до 2000 событий / 6 MiB и 2 MiB overhead. Полная очередь вытесняет старые события; счётчик пропусков виден в UI. Network reader продолжает обрабатывать Ping/Close. |
| gRPC UI | Общий бюджет строк для unary/reflection/stream 32 MiB; история до 2000 записей и 24 MiB на stream. Одно большее preview допускается отдельно внутри общего бюджета. Счётчики и активные соединения сохраняются при освобождении старых previews. |
| WebSocket UI | Отдельный общий бюджет строк 32 MiB, включая открытый saved preview; история до 2000 записей / 24 MiB на соединение. |
| Timeline | Одновременно отображается окно из 50 записей. Выбранное text/hex/base64 preview ограничено 64 KiB исходных данных. |
| WebSocket SQLite history | До 16 сохранённых сессий / 32 MiB; одна запись до 8 MiB вместе с summary. Payloads включаются только по явному выбору и остаются ограниченными previews. |
| Schema/project work | До двух крупных offline операций одновременно; deadline 30 секунд. Проект ограничен 32 MiB JSON, schema sources/descriptor set — 16 MiB. Дополнительные структурные лимиты перечислены в [architecture.md](architecture.md). |

JS-бюджеты оценивают строки как `length * 2`; они не измеряют heap браузера.
Go-бюджеты суммируют размеры preview strings и ограниченный overhead; shared
strings могут учитываться повторно в очереди и истории. Квоты предупреждают
неограниченное удержание результатов, но не заменяют измерение пиковых
allocations и RSS при конкретном сценарии.

## Воспроизводимые benchmarks

Из корня репозитория, с Go из `go.mod`:

```bash
go test ./internal/app -run '^$' \
  -bench '^BenchmarkWebSocketBurst10000$' -benchtime=1x -count=1 -benchmem

go test ./internal/app -run '^$' \
  -bench '^Benchmark.*Preview64KiB$' -benchtime=100x -count=1 -benchmem

go test ./internal/app -run '^$' \
  -bench '^Benchmark(WebSocketBurst10000|.*Preview64KiB)$' \
  -benchtime=1x -count=1 -benchmem \
  -o /tmp/relay-app-bench.test \
  -cpuprofile /tmp/relay-stream.cpu.pprof \
  -memprofile /tmp/relay-stream.heap.pprof

go tool pprof -top /tmp/relay-app-bench.test /tmp/relay-stream.cpu.pprof
go tool pprof -top -alloc_space /tmp/relay-app-bench.test /tmp/relay-stream.heap.pprof
```

`BenchmarkWebSocketBurst10000` создаёт text/base64 preview для 10 000 логических
сообщений и вызывает настоящую очередь/историю с mutex. Входные данные —
фиксированный public payload размером 1 KiB или 64 KiB. Сеть, WebKit и React в
этот benchmark не входят. `retained-budget-B` — учётный размер очереди и истории
последней итерации; `B/op` — все allocations на полный burst, включая уже
освобождённые буферы. Их нельзя сравнивать как два измерения RSS.

Preview benchmarks отдельно измеряют text/base64/redaction для WebSocket и
ProtoJSON/base64/redaction для динамического gRPC message. Protobuf wire decoding
и network I/O уже произошли до этой стадии и не включены в время. Вариант
`secret-true` использует фиксированный публичный canary, чтобы проверить стоимость
маскирования. Тесты и профили не используют реальные пользовательские данные.

### Контрольный результат 2026-10-25

Linux amd64, Go 1.26.10, Intel Core i7-12700H, `GOMAXPROCS=20`. Один burst-прогон
с CPU/heap profile дал следующие значения. Время зависит от нагрузки машины;
это локальная контрольная точка, а не переносимый performance threshold.

| Сценарий | Время / burst | Учётный retained размер | Pending / history | Все allocations / burst |
| --- | --- | --- | --- | --- |
| 10 000 × 1 KiB | 35.57 ms | 5.05 MiB | 64 / 2000 | 57 512 272 B |
| 10 000 × 64 KiB | 1.031 s | 13.87 MiB | 54 / 41 | 3 119 260 360 B |

Вторая строка обрабатывает 625 MiB логического payload, последовательно создавая
и освобождая previews. Число сохранённых событий сокращается по byte limit;
оставшиеся события не накапливаются пропорционально входному burst. Из 10 000
событий в pending queue вытеснено соответственно 9936 и 9946.

Отдельный прогон preview по 100 итераций на той же цепочке:

| Preview 64 KiB | Среднее время | B/op | allocs/op |
| --- | --- | --- | --- |
| WebSocket, без известного секрета | 122 µs | 311 345 | 6 |
| WebSocket, canary маскируется | 301 µs | 418 858 | 56 |
| gRPC, без известного секрета | 769 µs | 752 203 | 31 |
| gRPC, canary маскируется | 901 µs | 965 588 | 73 |

Профилирование выявило лишнее копирование всего сообщения `[]byte` в `string`
в общей проверке известных секретов. Проверка теперь использует `bytes.Contains`
с прежней семантикой поиска byte-последовательности, включая encoded variants.
Контрольный before/after убрал около 625 MiB cumulative allocations на burst
10 000 × 64 KiB без изменения retained budget. В итоговом `alloc_space` основные
источники — base64 encoding и ограниченные текстовые previews. Профили содержат
только fixture payload и создаются вне репозитория; команды выше воспроизводят
их без сохранения больших бинарных артефактов в git.

## Shutdown и проверка ресурсов

Package helper `app.Shutdown` запускает остановку HTTP, gRPC и WebSocket
параллельно. HTTP/gRPC сразу отменяют операции; WebSocket посылает 1001 Going Away
и ограничивает graceful close одной секундой, после чего закрывает физический
socket. Остановка ждёт workers и зарегистрированные Send/Ping/Close команды;
новый сетевой запуск и новые file grants отклоняются. Затем очищаются response
spools, preview records, cookie jar, file grants, reflection cache, session history
и известные secret snapshots. Повторные и конкурентные вызовы допустимы.
Desktop отменяет общий application context после этого helper, сохраняя
возможность отправить close frame; offline schema/project work получает общую
cancellation. Закрытие SQLite и снятие profile lock следуют за остановкой shell.

```bash
go test -race ./internal/app \
  -run 'TestShutdown|TestHTTPShutdown|TestWebSocketShutdown' -count=3
go test -race ./...
go vet ./...
```

Смешанный integration test одновременно держит HTTP download, завершённый
HTTP spool, gRPC stream с полной неотвеченной UI queue и WebSocket peer, который
не читает close frame. Два конкурентных shutdown должны закончиться в пределах
3 секунд; HTTP cancellation проверяется отдельно до окончания WebSocket grace.
Проверяются отмена на gRPC server, удаление обоих spools, освобождение доступных
previews, отклонение новых операций и повторная остановка. Отдельный cooperative
WebSocket fixture подтверждает реальный close code 1001. Подсчёт глобальных
goroutines не используется: resolver и test servers имеют независимый lifecycle.

Эта временная граница относится к сетевым transport и context-aware test doubles.
Синхронный системный keyring prompt не прерывается Go context в upstream API:
если worker уже вошёл в такой вызов, его и shutdown приходится ждать. После
остановки новый HTTP вызов отклоняется до загрузки workspace или обращения к
keyring. Освобождение ссылок не является гарантированным обнулением памяти Go/JS.
Аварийное завершение процесса также может оставить spool во временном каталоге;
штатные Forget, eviction и shutdown удаляют его.
