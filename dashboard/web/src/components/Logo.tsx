// Logo is the landing page's three-arc Orkano mark (also the favicon).
// Decorative by default: pair it with visible text wherever it appears.
export function Logo({
  size = 22,
  className,
}: {
  size?: number;
  className?: string;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="21 20 245 245"
      fill="none"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={className}
    >
      <g transform="translate(48,5)" stroke="#00A88F" strokeWidth="42">
        <path d="M 59 199.1 A 74 74 0 0 1 50.4 76.7" />
        <path d="M 106.3 61.7 A 74 74 0 0 1 169.3 124.7" />
        <path d="M 154.3 180.6 A 74 74 0 0 1 116.4 206.1" />
      </g>
    </svg>
  );
}
