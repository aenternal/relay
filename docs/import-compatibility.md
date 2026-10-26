# Совместимость импорта проектов

Импорт работает локально в Go. Preview показывает ограниченный отчёт, а применение создаёт новый workspace со свежими ID. Импорт не отправляет запросы, не открывает исходные пути и не выполняет scripts, tests, flows или template plugins. Неподдерживаемые данные перечисляются в предупреждениях; исходные значения и содержимое scripts в отчёт не попадают.

## Postman Collection v2.1

Принимается JSON Collection v2.1 с `info.schema`, указывающим на официальную схему этой версии, и массивом `item`. Другие версии, API-обёртка `{collection: ...}`, отдельные environment exports и произвольный JSON отклоняются. Структура сверена с [официальной схемой Collection v2.1](https://schema.postman.com/collection/json/v2.1.0/draft-07/collection.json).

| Данные | Результат в Relay |
| --- | --- |
| Collection, вложенные folders, HTTP requests | Одна collection с сохранением дерева и порядка запросов |
| URL string/object, method, query, path variables | HTTP definition; повторяющиеся query и выключенные строки сохраняются |
| `url.query` вместе с `url.raw` | Структурированный query заменяет raw query; отчёт объясняет выбор |
| Headers array или строка | Сохраняются порядок, повторы и `disabled` |
| Auth `noauth`, basic, bearer, API key | Соответствующие auth helpers; отсутствие auth наследуется от folder/collection |
| Collection variables | Environment; строки, числа и boolean преобразуются в текст без потери записи больших чисел |
| Raw text/JSON, urlencoded, multipart text | Соответствующие body editors |
| Files, GraphQL, scripts/tests, examples/responses, scoped variables, внешние TLS/proxy/runtime settings | Данные пропускаются с предупреждениями; файлы нужно выбрать заново |

## Insomnia JSON export v4

Принимается legacy JSON с `_type: "export"`, `__export_format: 4` и плоским `resources`. Требуется ровно один workspace; для нескольких workspace нужны отдельные файлы. YAML v5 и другие версии отклоняются. Официальная документация описывает [различия v4 JSON и v5 YAML](https://developer.konghq.com/insomnia/import-export/), а [пример legacy v4 в репозитории Kong](https://github.com/Kong/insomnia/discussions/7866) показывает resource links и поля HTTP request.

| Данные | Результат в Relay |
| --- | --- |
| `workspace`, `request_group`, `request` | Одна collection; вложенность восстанавливается независимо от порядка resources |
| `environment` и цепочки parent environments | Отдельные environments с объединением значений от родителей; дочерние значения имеют приоритет |
| HTTP method/URL, headers, parameters, pathParameters | Сохраняются повторы и `disabled`; query из URL и parameters объединяются с предупреждением |
| Basic, bearer с обычным префиксом, API key, none/inherit | Соответствующие auth helpers |
| Text/JSON, urlencoded, multipart text | Соответствующие body editors |
| `{{name}}`, `{{ name }}`, `{{ _.name }}` | Простые ссылки `{{name}}` |
| Nunjucks tags/filters, response chaining, dynamic expressions | Пустая переменная и предупреждение; выражения не исполняются |
| Другие resource types, scripts, folder environments, files, нестандартный auth/runtime settings | Пропускаются с явным отчётом |

## Секреты, тела и ограничения

Literal credentials из auth, чувствительных headers/query и переменных с именами вроде `token`, `password`, `secret` заменяются пустыми secret variables. Их нужно заполнить через keyring в новом workspace. Public variable, используемая как credential, также становится пустой secret variable. Копии известных credential literals убираются из остальных текстовых полей, включая body и public variables. Keyring references, локальные file grants, исходные пути файлов и response history не переносятся.

Body values попадают в сохранённый проект только при явном выборе **Include bodies**; по умолчанию raw body, form values и multipart text values удаляются. Это не анализатор произвольного содержимого: неизвестный секрет внутри обычного body или переменной с нейтральным именем может сохраниться при явном включении bodies.

В JSON-телах Relay подставляет переменные только внутри строк. Тела с unquoted templates, например `{"count":{{count}}}`, сохраняются для ручного исправления и получают предупреждение `json-body-review`; импорт не превращает их в исполняемый template. Для обычного raw-текста подстановка остаётся текстовой.

Файл ограничен 32 MiB, JSON — глубиной 32 и 200 000 tokens; повторяющиеся JSON keys запрещены. Импорт ограничен 2 000 HTTP requests, 10 000 records/variables, 1 024 строками каждого HTTP field list и глубиной дерева 24. Отчёт содержит максимум 200 предупреждений; последнее сообщает число пропущенных предупреждений. Для удаления копий credentials допускаются максимум 1 024 разных литерала суммарно до 1 MiB; превышение отклоняет весь импорт. Циклы, отсутствующие родители, повторяющиеся resource IDs, неверные типы и неоднозначные версии отклоняются до применения.

Тестовые [Postman fixture](../testdata/project/postman-v21.json) и [Insomnia fixture](../testdata/project/insomnia-v4.json) созданы для Relay и содержат только вымышленные адреса и canary values. Они покрывают вложенность, отключённые/повторяющиеся поля, наследование environments, большие числа, auth, body modes и диагностику потерь. Негативные проверки охватывают версии, orphan/cycle/duplicate ID, dynamic templates, неоднозначный JSON и превышение лимитов.
