/** @type {import('tailwindcss').Config} */
//
// Design tokens are kept as CSS custom properties in src/design/tokens.css.
// This config exposes them via Tailwind so utilities like `bg-bg-card` and
// `text-ink-3` work without re-stating the values. Light/dark switch is the
// `class` strategy: <html class="dark"> toggles the dark token set.
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        bg: "var(--bg)",
        "bg-elev": "var(--bg-elev)",
        "bg-card": "var(--bg-card)",
        line: "var(--line)",
        "line-strong": "var(--line-strong)",
        ink: "var(--ink)",
        "ink-2": "var(--ink-2)",
        "ink-3": "var(--ink-3)",
        "ink-4": "var(--ink-4)",
        "ink-5": "var(--ink-5)",
        accent: "var(--accent)",
        "accent-soft": "var(--accent-soft)",
        amber: "var(--amber)",
        "amber-soft": "var(--amber-soft)",
        crit: "var(--crit)",
        "crit-soft": "var(--crit-soft)",
        good: "var(--good)",
        "good-soft": "var(--good-soft)",
      },
      fontFamily: {
        sans: ["Geist", "ui-sans-serif", "system-ui", "-apple-system", "sans-serif"],
        mono: ["Geist Mono", "ui-monospace", "JetBrains Mono", "monospace"],
        display: ["Fraunces", "Geist", "serif"],
      },
      borderRadius: {
        "z-sm": "var(--r-sm)",
        "z-md": "var(--r-md)",
        "z-lg": "var(--r-lg)",
      },
      spacing: {
        rail: "var(--rail)",
        "rail-l": "var(--left)",
      },
      keyframes: {
        fadeUp: {
          from: { opacity: "0", transform: "translateY(6px)" },
          to: { opacity: "1", transform: "none" },
        },
        fadeIn: {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        pulse: {
          "0%": { boxShadow: "0 0 0 0 rgba(53,83,216,.35)" },
          "100%": { boxShadow: "0 0 0 10px rgba(53,83,216,0)" },
        },
        injectIn: {
          "0%": { opacity: "0", transform: "translateY(-10px)" },
          "100%": { opacity: "1", transform: "translateY(0)" },
        },
        slideInRight: {
          "0%": { transform: "translateX(24px)", opacity: "0" },
          "100%": { transform: "translateX(0)", opacity: "1" },
        },
        subIn: {
          "0%": { opacity: "0", transform: "translateY(4px)" },
          "100%": { opacity: "1", transform: "translateY(0)" },
        },
      },
      animation: {
        "fade-up": "fadeUp .42s cubic-bezier(.2,.7,.2,1) both",
        "fade-in": "fadeIn .3s ease both",
        pulse: "pulse 2.4s ease-out infinite",
        "inject-in": "injectIn .45s cubic-bezier(.2,.8,.2,1) both",
        "slide-in-right": "slideInRight .26s cubic-bezier(.2,.7,.2,1) both",
        "sub-in": "subIn .26s cubic-bezier(.2,.7,.2,1) both",
      },
    },
  },
  plugins: [],
};
