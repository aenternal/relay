import { useState } from 'react';
import type { ProtoMessage, ProtoMethod, ProtoSchemaDetail } from '../lib/types';
import { rpcKind } from '../lib/types';

export function ProtoMessageFields({ message }: { message: ProtoMessage | undefined }) {
  const [paging, setPaging] = useState({ name: '', page: 0 });
  if (!message) return <p className="hint">Выберите сообщение, чтобы посмотреть поля.</p>;
  const page = paging.name === message.name ? paging.page : 0;
  const pageSize = 100;
  return (
    <div className="proto-message-fields">
      <h3>
        <code>{message.name}</code>
      </h3>
      {message.comment && <p className="proto-comment">{message.comment}</p>}
      {!message.fields.length ? (
        <p className="hint">Сообщение не содержит полей.</p>
      ) : (
        <div className="proto-field-table">
          <table>
            <thead>
              <tr>
                <th>Поле</th>
                <th>Тип</th>
                <th>Правила</th>
              </tr>
            </thead>
            <tbody>
              {message.fields.slice(page * pageSize, (page + 1) * pageSize).map((field) => (
                <tr key={field.number}>
                  <td>
                    <code>{field.jsonName}</code>
                    <small>
                      #{field.number} · {field.name}
                    </small>
                    {field.comment && <p className="proto-comment">{field.comment}</p>}
                  </td>
                  <td>
                    <code>
                      {field.map ? `map<${field.mapKey}, ${field.mapValue}>` : field.type}
                    </code>
                    {field.enumValues.length > 0 && (
                      <small>
                        {field.enumValues
                          .slice(0, 100)
                          .map((value) => `${value.name} = ${value.number}`)
                          .join(', ')}
                        {field.enumValues.length > 100 && ` … ещё ${field.enumValues.length - 100}`}
                      </small>
                    )}
                  </td>
                  <td>
                    {[
                      field.required && 'required',
                      field.repeated && !field.map && 'repeated',
                      field.presence && 'presence',
                      field.oneof && `oneof ${field.oneof}`,
                    ]
                      .filter(Boolean)
                      .join(' · ') || '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {message.fields.length > pageSize && (
            <div className="proto-pagination">
              <button
                className="button small"
                disabled={page === 0}
                onClick={() => setPaging({ name: message.name, page: page - 1 })}
              >
                Предыдущие поля
              </button>
              <span>
                {page * pageSize + 1}–{Math.min((page + 1) * pageSize, message.fields.length)} из{' '}
                {message.fields.length}
              </span>
              <button
                className="button small"
                disabled={(page + 1) * pageSize >= message.fields.length}
                onClick={() => setPaging({ name: message.name, page: page + 1 })}
              >
                Следующие поля
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function ProtoSchemaDetails({
  detail,
  selectMethod,
}: {
  detail: ProtoSchemaDetail;
  selectMethod?: (method: ProtoMethod) => void;
}) {
  const [selectedMethod, setSelectedMethod] = useState('');
  const [selectedMessage, setSelectedMessage] = useState('');
  const [serviceSearch, setServiceSearch] = useState('');
  const [messageSearch, setMessageSearch] = useState('');
  const methods = detail.services.flatMap((service) => service.methods);
  const method = methods.find((entry) => entry.fullMethod === selectedMethod);
  const messages = detail.messages;
  const matchingMethods = methods.filter((entry) =>
    entry.fullMethod.toLocaleLowerCase().includes(serviceSearch.toLocaleLowerCase()),
  );
  const visibleMethods = matchingMethods.slice(0, 200);
  const visibleServices = detail.services
    .filter(
      (service) =>
        visibleMethods.some((entry) => entry.service === service.name) ||
        (!service.methods.length &&
          service.name.toLocaleLowerCase().includes(serviceSearch.toLocaleLowerCase())),
    )
    .slice(0, 100);
  const filteredMessages = messages.filter((entry) =>
    entry.name.toLocaleLowerCase().includes(messageSearch.toLocaleLowerCase()),
  );
  const visibleMessages = filteredMessages.slice(0, 500);
  const packageNames = [
    ...new Set(visibleServices.map((service) => service.name.split('.').slice(0, -1).join('.'))),
  ];
  return (
    <div className="proto-schema-details">
      <div className="proto-browser">
        <div className="proto-service-tree" aria-label="Дерево сервисов Protobuf">
          <h3>Сервисы</h3>
          <input
            aria-label="Поиск методов в схеме"
            placeholder="Найти метод…"
            value={serviceSearch}
            onChange={(event) => setServiceSearch(event.target.value)}
          />
          {matchingMethods.length > 200 && (
            <p className="hint">Показаны первые 200 методов. Уточните поиск.</p>
          )}
          {packageNames.map((packageName) => (
            <details open key={packageName} className="proto-package">
              <summary>
                <code>{packageName || '(без package)'}</code>
              </summary>
              {visibleServices
                .filter((service) => service.name.split('.').slice(0, -1).join('.') === packageName)
                .map((service) => (
                  <details open key={service.name} className="proto-service">
                    <summary title={service.comment}>{service.name.split('.').at(-1)}</summary>
                    {visibleMethods
                      .filter((entry) => entry.service === service.name)
                      .map((entry) => (
                        <button
                          key={entry.fullMethod}
                          className={`proto-method ${entry.fullMethod === selectedMethod ? 'selected' : ''}`}
                          aria-label={`Метод ${entry.fullMethod}`}
                          onClick={() => {
                            setSelectedMethod(entry.fullMethod);
                            setSelectedMessage(entry.input);
                          }}
                        >
                          <span>{entry.name}</span>
                          <small>{rpcKind(entry)}</small>
                        </button>
                      ))}
                  </details>
                ))}
            </details>
          ))}
          {!detail.services.length && (
            <p className="hint">В схеме нет сервисов. Сообщения доступны ниже.</p>
          )}
        </div>
        <div className="proto-method-info">
          {method ? (
            <>
              <h3>
                <code>{method.fullMethod}</code>
              </h3>
              <span className="active-badge">{rpcKind(method)}</span>
              {method.comment && <p className="proto-comment">{method.comment}</p>}
              <dl className="proto-type-pair">
                <div>
                  <dt>Input</dt>
                  <dd>
                    <button
                      className="text-button"
                      onClick={() => setSelectedMessage(method.input)}
                    >
                      {method.input}
                    </button>
                  </dd>
                </div>
                <div>
                  <dt>Output</dt>
                  <dd>
                    <button
                      className="text-button"
                      onClick={() => setSelectedMessage(method.output)}
                    >
                      {method.output}
                    </button>
                  </dd>
                </div>
              </dl>
              {selectMethod && (
                <button className="button primary small" onClick={() => selectMethod(method)}>
                  Создать gRPC запрос
                </button>
              )}
            </>
          ) : (
            <p className="hint">Выберите метод в дереве, чтобы увидеть типы и streaming flags.</p>
          )}
          <label className="form-field">
            Поиск типов
            <input
              aria-label="Поиск типов Protobuf"
              placeholder="Полное имя сообщения…"
              value={messageSearch}
              onChange={(event) => setMessageSearch(event.target.value)}
            />
          </label>
          {filteredMessages.length > 500 && (
            <p className="hint">Показаны первые 500 типов. Уточните поиск.</p>
          )}
          <label className="form-field">
            Сообщение
            <select
              aria-label="Сообщение Protobuf"
              value={selectedMessage}
              onChange={(event) => setSelectedMessage(event.target.value)}
            >
              <option value="">Выберите тип…</option>
              {selectedMessage &&
                !visibleMessages.some((message) => message.name === selectedMessage) && (
                  <option value={selectedMessage}>{selectedMessage}</option>
                )}
              {visibleMessages.map((message) => (
                <option key={message.name} value={message.name}>
                  {message.name}
                </option>
              ))}
            </select>
          </label>
          <ProtoMessageFields
            message={messages.find((message) => message.name === selectedMessage)}
          />
        </div>
      </div>
      <details className="proto-files">
        <summary>Файлы ({detail.files.length})</summary>
        <ul>
          {detail.files.map((file) => (
            <li key={file.name}>
              <code>{file.name}</code>
              <span>
                {file.syntax} · {file.package || '(без package)'}
              </span>
              {file.imports.length > 0 && <small>imports: {file.imports.join(', ')}</small>}
            </li>
          ))}
        </ul>
      </details>
    </div>
  );
}
