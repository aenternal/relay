// Format JSON without converting number tokens (including 64-bit integers) to JS numbers.
export function prettyJSON(value: string): string {
  if (value.length > 1024 * 1024) return value;
  try {
    JSON.parse(value);
  } catch {
    return value;
  }
  const maximumOutput = 2 * 1024 * 1024;
  let output = '',
    depth = 0,
    quoted = false,
    escaped = false;
  const newline = () => {
    output += '\n' + '  '.repeat(depth);
  };
  for (const character of value) {
    if (output.length > maximumOutput) return value;
    if (quoted) {
      output += character;
      if (escaped) escaped = false;
      else if (character === '\\') escaped = true;
      else if (character === '"') quoted = false;
      continue;
    }
    if (character === '"') {
      quoted = true;
      output += character;
    } else if (character === '{' || character === '[') {
      output += character;
      depth++;
      if (depth > 100) return value;
      newline();
    } else if (character === '}' || character === ']') {
      depth--;
      newline();
      output += character;
    } else if (character === ',') {
      output += character;
      newline();
    } else if (character === ':') output += ': ';
    else if (!/\s/.test(character)) output += character;
  }
  return output.length <= maximumOutput ? output : value;
}

export function responseHex(base64: string, maximum = 65536, text = ''): string {
  let bytes: string;
  let limited = false;
  try {
    if (base64) {
      // Decode only the portion needed for the hex view.
      const chunk = base64.slice(0, Math.ceil(maximum / 3) * 4);
      bytes = atob(chunk);
      limited = base64.length > chunk.length || bytes.length > maximum;
    } else {
      const encoded = new TextEncoder().encode(text.slice(0, maximum));
      bytes = Array.from(encoded.subarray(0, maximum), (value) => String.fromCharCode(value)).join(
        '',
      );
      limited = encoded.length > maximum || text.length > maximum;
    }
  } catch {
    return 'Не удалось декодировать бинарное тело.';
  }
  const rows: string[] = [];
  for (let offset = 0; offset < Math.min(bytes.length, maximum); offset += 16) {
    const segment = bytes.slice(offset, Math.min(offset + 16, maximum));
    const hex = Array.from(segment, (character) =>
      character.charCodeAt(0).toString(16).padStart(2, '0'),
    ).join(' ');
    const printable = Array.from(segment, (character) =>
      /[\x20-\x7e]/.test(character) ? character : '.',
    ).join('');
    rows.push(`${offset.toString(16).padStart(8, '0')}  ${hex.padEnd(47)}  ${printable}`);
  }
  if (limited || bytes.length > maximum)
    rows.push(`\nHex: показаны первые ${maximum} байт превью.`);
  return rows.join('\n');
}
