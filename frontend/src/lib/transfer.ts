export const PROJECT_MAX_BYTES = 32 * 1024 * 1024;

export function projectInputError(data: string): string {
  if (!data.trim()) return 'Выберите файл или вставьте JSON проекта.';
  if (data.length > PROJECT_MAX_BYTES || new TextEncoder().encode(data).length > PROJECT_MAX_BYTES)
    return 'Размер проекта превышает 32 МиБ.';
  return '';
}

export function downloadProject(filename: string, data: string) {
  const url = URL.createObjectURL(new Blob([data], { type: 'application/json;charset=utf-8' }));
  const link = document.createElement('a');
  link.href = url;
  link.download =
    Array.from(filename, (character) =>
      character.charCodeAt(0) < 32 || character === '/' || character === '\\' ? '_' : character,
    ).join('') || 'relay-project.json';
  document.body.append(link);
  link.click();
  link.remove();
  // WebKit may consume the download URL after the click handler returns.
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}
