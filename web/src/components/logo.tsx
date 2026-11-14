// The seat-hub mark: four seats around a hub, one of them yours. The other
// three are drawn lighter. Colour comes from currentColor, so it follows the
// theme; public/favicon.svg and docs/logo.svg are fixed-colour copies.
const SEAT = 'M7 4H11.5A3 3 0 0 1 14.5 7V11.23A5 5 0 0 0 11.23 14.5H7A3 3 0 0 1 4 11.5V7A3 3 0 0 1 7 4Z'

export function LogoMark({ size = 20, className }: { size?: number; className?: string }) {
  return <svg viewBox="0 0 32 32" width={size} height={size} fill="currentColor" className={className} aria-hidden="true" focusable="false">
    <path d={SEAT} />
    <g opacity=".42"><path d={SEAT} transform="rotate(90 16 16)" /><path d={SEAT} transform="rotate(180 16 16)" /><path d={SEAT} transform="rotate(270 16 16)" /></g>
    <circle cx="16" cy="16" r="2.9" />
  </svg>
}
