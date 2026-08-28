/**
 * Mirrors the config the three pages used to hand the Tailwind play CDN.
 * Keep `content` pointed at every file that can carry a class name — Tailwind
 * scans raw text, so classes assembled in JS string literals (stats.html builds
 * `boxClass`/`labelClass`/`valueClass` that way) are picked up as long as the
 * literal appears in the file. A class built by concatenation would NOT be.
 */
module.exports = {
  darkMode: 'class',
  content: ['./server/wwwroot/*.html'],
  theme: {
    extend: {
      fontFamily: { sans: ['Inter', 'system-ui', '-apple-system', 'sans-serif'] },
      animation: {
        'fade-in': 'fadeIn 0.3s ease-in-out',
        'slide-up': 'slideUp 0.4s ease-out',
      },
      keyframes: {
        fadeIn: { '0%': { opacity: '0' }, '100%': { opacity: '1' } },
        slideUp: { '0%': { transform: 'translateY(10px)', opacity: '0' }, '100%': { transform: 'translateY(0)', opacity: '1' } },
      },
    },
  },
};
