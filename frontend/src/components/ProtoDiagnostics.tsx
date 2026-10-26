import type { ProtoDiagnostic } from '../lib/types';

export function ProtoDiagnostics({
  diagnostics,
  warnings = [],
}: {
  diagnostics: ProtoDiagnostic[];
  warnings?: string[];
}) {
  if (!diagnostics.length && !warnings.length) return null;
  return (
    <ul className="proto-diagnostics" aria-label="Диагностика Protobuf">
      {diagnostics.map((item, index) => (
        <li key={`error-${index}`} role="alert">
          <code>
            {[item.file, item.line > 0 ? `${item.line}:${item.column}` : '', item.path]
              .filter(Boolean)
              .join(' · ')}
          </code>
          {item.message}
        </li>
      ))}
      {warnings.map((item, index) => (
        <li className="warning" key={`warning-${index}`}>
          {item}
        </li>
      ))}
    </ul>
  );
}
