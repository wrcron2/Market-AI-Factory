import type { Config } from 'tailwindcss'

/**
 * MarketFlow AI — design tokens. preflight is ON: this is now the only stylesheet.
 */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        base: '#0f1117',
        surface: {
          DEFAULT: '#1e293b',
          hover: '#1a2236',
          raised: '#161e2e',
          sunken: '#141b29',
        },
        line: {
          DEFAULT: '#334155',
          soft: '#28344a',
          faint: '#1d2738',
        },
        ink: {
          DEFAULT: '#e2e8f0',
          muted: '#94a3b8',
          faint: '#64748b',
        },
        signal: {
          blue: '#3b82f6',
          green: '#22c55e',
          red: '#ef4444',
          orange: '#f97316',
          yellow: '#eab308',
          purple: '#7c3aed',
        },
      },
      fontFamily: {
        sans: ['Geist', 'ui-sans-serif', 'system-ui', '-apple-system', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'monospace'],
      },
      borderColor: { DEFAULT: '#334155' },
      keyframes: {
        'pulse-dot': { '0%,100%': { opacity: '1' }, '50%': { opacity: '.35' } },
        'slide-in': {
          from: { transform: 'translateX(24px)', opacity: '0' },
          to: { transform: 'translateX(0)', opacity: '1' },
        },
        glow: {
          '0%,100%': { boxShadow: '0 0 0 0 rgba(239,68,68,.45)' },
          '50%': { boxShadow: '0 0 22px 2px rgba(239,68,68,.65)' },
        },
        spin: { to: { transform: 'rotate(360deg)' } },
      },
      animation: {
        'pulse-dot': 'pulse-dot 1.6s ease-in-out infinite',
        'slide-in': 'slide-in .25s ease',
        glow: 'glow 1.1s ease-in-out infinite',
      },
    },
  },
  plugins: [],
} satisfies Config
