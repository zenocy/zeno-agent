interface Props {
  onClick?: () => void;
  title?: string;
}

export function PinIcon({ onClick, title = "Pinned · click to unpin" }: Props) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      aria-label={title}
      className="bg-transparent border-0 cursor-pointer text-accent w-[18px] h-[18px] grid place-items-center rounded-[3px] opacity-55 group-hover/widget:opacity-100 hover:!opacity-100 hover:bg-bg transition-opacity"
    >
      <svg
        viewBox="0 0 14 14"
        width="12"
        height="12"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.2"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M9.2 1.5 L12.5 4.8 L10.5 5.4 L8.8 8 L6 5.2 L8.6 3.5 Z" />
        <path d="M6 5.2 L2.5 8.7" />
        <path d="M5.5 5.7 L4.2 7" />
      </svg>
    </button>
  );
}
