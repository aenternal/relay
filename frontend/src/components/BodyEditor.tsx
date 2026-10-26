import { useEffect, useRef } from 'react';
import { basicSetup } from 'codemirror';
import { EditorView } from '@codemirror/view';
import { json } from '@codemirror/lang-json';

export function BodyEditor({
  value,
  onChange,
  label = 'Тело запроса',
}: {
  value: string;
  onChange: (value: string) => void;
  label?: string;
}) {
  const parent = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const change = useRef(onChange);
  change.current = onChange;
  useEffect(() => {
    const editor = new EditorView({
      doc: value,
      parent: parent.current!,
      extensions: [
        basicSetup,
        json(),
        EditorView.lineWrapping,
        EditorView.contentAttributes.of({ 'aria-label': label, spellcheck: 'false' }),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) change.current(update.state.doc.toString());
        }),
        EditorView.theme({
          '&': { height: '100%', color: 'var(--text)', backgroundColor: 'var(--surface)' },
          '.cm-scroller': { overflow: 'auto', fontFamily: 'var(--mono)', fontSize: '13px' },
          '.cm-content': { padding: '16px 0', caretColor: 'var(--accent)' },
          '.cm-gutters': {
            backgroundColor: 'var(--surface)',
            color: 'var(--muted)',
            border: 'none',
          },
          '.cm-activeLine, .cm-activeLineGutter': { backgroundColor: 'var(--hover)' },
          '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': {
            backgroundColor: 'var(--selection)',
          },
          '.cm-cursor': { borderLeftColor: 'var(--text)' },
          '&.cm-focused': { outline: 'none' },
        }),
      ],
    });
    view.current = editor;
    return () => {
      editor.destroy();
      view.current = null;
    };
    // The editor belongs to this request tab. Updates use the separate effect.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    const editor = view.current;
    if (editor && editor.state.doc.toString() !== value)
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
  }, [value]);
  return <div className="body-editor" ref={parent} />;
}
