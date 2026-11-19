import React, { useEffect, useRef } from 'react';

export interface ContextMenuItem {
  label: string;
  shortcut?: string;
  danger?: boolean;
  separator?: boolean;
  disabled?: boolean;
  onClick?: () => void;
}

interface ContextMenuProps {
  x: number;
  y: number;
  title?: string;
  items: ContextMenuItem[];
  onClose: () => void;
}

export const ContextMenu: React.FC<ContextMenuProps> = ({
  x,
  y,
  title,
  items,
  onClose,
}) => {
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        onClose();
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('mousedown', onDown, true);
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('mousedown', onDown, true);
      window.removeEventListener('keydown', onKey);
    };
  }, [onClose]);

  // Keep menu inside viewport bounds.
  const clampedX = Math.min(x, window.innerWidth - 230);
  const clampedY = Math.min(y, window.innerHeight - Math.min(360, items.length * 32 + 40));

  return (
    <div
      ref={ref}
      className="ctx-menu"
      style={{ left: Math.max(8, clampedX), top: Math.max(8, clampedY) }}
      onContextMenu={(e) => e.preventDefault()}
    >
      {title && <div className="ctx-title">{title}</div>}
      {items.map((item, idx) =>
        item.separator ? (
          <div key={`sep-${idx}`} className="ctx-sep" />
        ) : (
          <button
            key={`${item.label}-${idx}`}
            type="button"
            className={`ctx-item ${item.danger ? 'danger' : ''}`}
            disabled={item.disabled}
            onClick={() => {
              item.onClick?.();
              onClose();
            }}
          >
            <span>{item.label}</span>
            {item.shortcut && <span className="ctx-shortcut">{item.shortcut}</span>}
          </button>
        ),
      )}
    </div>
  );
};
