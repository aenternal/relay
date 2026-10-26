import { useRef } from 'react';

export function PanelSeparator({
  label,
  orientation,
  value,
  min,
  max,
  onChange,
}: {
  label: string;
  orientation: 'horizontal' | 'vertical';
  value: number;
  min: number;
  max: number;
  onChange: (value: number) => void;
}) {
  const drag = useRef<{ id: number; coordinate: number; value: number } | null>(null);
  const coordinate = (event: { clientX: number; clientY: number }) =>
    orientation === 'vertical' ? event.clientX : event.clientY;
  const change = (next: number) => onChange(Math.min(max, Math.max(min, next)));
  return (
    <div
      className={`panel-separator ${orientation}`}
      role="separator"
      aria-label={label}
      aria-orientation={orientation}
      aria-valuemin={min}
      aria-valuemax={max}
      aria-valuenow={value}
      aria-valuetext={`${value} пикселей`}
      tabIndex={0}
      onDoubleClick={() => change(orientation === 'vertical' ? 260 : 320)}
      onKeyDown={(event) => {
        const backward = orientation === 'vertical' ? 'ArrowLeft' : 'ArrowUp';
        const forward = orientation === 'vertical' ? 'ArrowRight' : 'ArrowDown';
        if (![backward, forward, 'Home', 'End'].includes(event.key)) return;
        event.preventDefault();
        if (event.key === 'Home') change(min);
        else if (event.key === 'End') change(max);
        else change(value + (event.key === backward ? -1 : 1) * (event.shiftKey ? 50 : 20));
      }}
      onPointerDown={(event) => {
        if (event.button !== 0) return;
        event.preventDefault();
        event.currentTarget.focus();
        event.currentTarget.setPointerCapture?.(event.pointerId);
        drag.current = { id: event.pointerId, coordinate: coordinate(event), value };
      }}
      onPointerMove={(event) => {
        const start = drag.current;
        if (start && start.id === event.pointerId)
          change(start.value + coordinate(event) - start.coordinate);
      }}
      onPointerUp={(event) => {
        if (drag.current?.id === event.pointerId) drag.current = null;
      }}
      onPointerCancel={() => {
        drag.current = null;
      }}
      onLostPointerCapture={() => {
        drag.current = null;
      }}
    />
  );
}
