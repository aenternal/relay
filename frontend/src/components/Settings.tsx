import { useState } from 'react';
import { useRelay } from '../lib/context';

export function Settings() {
  const { data, mutate, capabilities, buildInfo } = useRelay();
  const [theme, setTheme] = useState(data.settings.theme);
  const [preview, setPreview] = useState(String(data.settings.responsePreviewBytes));
  const [history, setHistory] = useState(String(data.settings.historyLimit));
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const valid =
    Number.isInteger(Number(preview)) &&
    Number(preview) >= 1024 &&
    Number(preview) <= 32 * 1024 * 1024 &&
    Number.isInteger(Number(history)) &&
    Number(history) >= 1 &&
    Number(history) <= 100000;
  return (
    <section className="page settings-page">
      <div className="page-heading">
        <div>
          <div className="eyebrow">ПРИЛОЖЕНИЕ</div>
          <h1>Настройки</h1>
          <p>Интерфейс и безопасные ограничения локального клиента.</p>
        </div>
      </div>
      {buildInfo && (
        <div className="settings-group" aria-label="О сборке">
          <h2>Relay {buildInfo.version}</h2>
          <p className="hint">
            {buildInfo.goos}/{buildInfo.goarch} · {buildInfo.goVersion}
          </p>
          <p className="hint">
            Исходная ревизия: <code>{buildInfo.commit}</code>
          </p>
          <p className="hint">
            {buildInfo.unsigned
              ? 'Сборка без цифровой подписи'
              : 'Сведения о подписи проверяйте по manifest релиза'}
          </p>
        </div>
      )}
      <form
        onSubmit={async (event) => {
          event.preventDefault();
          if (!valid || busy) return;
          setBusy(true);
          const result = await mutate((api) =>
            api.SaveSettings({
              theme,
              responsePreviewBytes: Number(preview),
              historyLimit: Number(history),
            }),
          );
          setBusy(false);
          setSaved(!!result);
        }}
      >
        <div className="settings-group">
          <h2>Внешний вид</h2>
          <div className="setting-row">
            <div>
              <strong>Тема интерфейса</strong>
              <p>Системная тема следует настройкам ОС.</p>
            </div>
            <select
              aria-label="Тема интерфейса"
              value={theme}
              onChange={(event) => {
                setTheme(event.target.value);
                setSaved(false);
              }}
            >
              <option value="system">Системная</option>
              <option value="dark">Тёмная</option>
              <option value="light">Светлая</option>
            </select>
          </div>
        </div>
        <div className="settings-group">
          <h2>Лимиты ответов</h2>
          <div className="setting-row">
            <div>
              <label htmlFor="preview-limit">Размер превью ответа, байт</label>
              <p>
                От 1 KiB до 32 MiB. По умолчанию 5 MiB.
                <br />
                Полное тело можно сохранить отдельно в файл.
              </p>
            </div>
            <input
              id="preview-limit"
              type="number"
              min={1024}
              max={32 * 1024 * 1024}
              step={1}
              value={preview}
              onChange={(event) => {
                setPreview(event.target.value);
                setSaved(false);
              }}
            />
          </div>
          <div className="setting-row">
            <div>
              <label htmlFor="history-limit">Количество событий в истории</label>
              <p>
                От 1 до 100 000. По умолчанию 2 000.
                <br />
                Для gRPC и WebSocket — до 2 000 сообщений; также действует лимит памяти.
              </p>
            </div>
            <input
              id="history-limit"
              type="number"
              min={1}
              max={100000}
              step={1}
              value={history}
              onChange={(event) => {
                setHistory(event.target.value);
                setSaved(false);
              }}
            />
          </div>
        </div>
        <div className="settings-group">
          <h2>Хранение данных</h2>
          <p className="hint">
            Коллекции и настройки сохраняются в локальной SQLite. Тела запросов — только при
            включении опции в редакторе. Секретные переменные хранятся отдельно.
          </p>
          <div className="setting-row">
            <div>
              <strong>Системное хранилище секретов</strong>
              <p>{capabilities.message || capabilities.secretBackend || 'Проверка доступности…'}</p>
            </div>
            <span className={capabilities.secretAvailable ? 'active-badge' : 'quiet-badge'}>
              {capabilities.secretAvailable ? 'Доступно' : 'Недоступно'}
            </span>
          </div>
        </div>
        <div className="form-footer">
          {saved && (
            <span className="success-text" role="status">
              Настройки сохранены
            </span>
          )}
          <button className="button primary" disabled={!valid || busy}>
            {busy ? 'Сохранение…' : 'Сохранить настройки'}
          </button>
        </div>
      </form>
    </section>
  );
}
