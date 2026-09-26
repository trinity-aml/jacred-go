/*
 * Shared shell for the jacred-go pages: one theme mechanism and one navigation.
 *
 * Loaded with `defer` from index.html, stats.html and settings.html. The
 * anti-flash part is NOT here — it has to run before the first paint, so each
 * page keeps an identical inline snippet in <head>; see themeBootSnippet below
 * for the text it must match.
 */
(function () {
  'use strict';

  var THEME_KEY = 'theme';

  /* ------------------------------------------------------------- theme --
   * The pages already agreed on Tailwind's `class` strategy (a `dark` class on
   * <html>), so every existing dark: utility keeps working untouched. What they
   * did not agree on was reading the stored choice back: stats and settings had
   * the boot snippet, index.html did not, so a theme picked on one page was
   * ignored by the other two until the browser was restarted.
   */
  function prefersDark() {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  }

  function stored() {
    try {
      return localStorage.getItem(THEME_KEY);
    } catch (e) {
      // Private mode, or site data blocked. Fall back to the system setting.
      return null;
    }
  }

  function isDark() {
    return document.documentElement.classList.contains('dark');
  }

  function apply(dark) {
    document.documentElement.classList.toggle('dark', dark);
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', dark ? '#0a0a0f' : '#f9fafb');
    document.querySelectorAll('[data-theme-icon="moon"]').forEach(function (el) {
      el.classList.toggle('hidden', !dark);
    });
    document.querySelectorAll('[data-theme-icon="sun"]').forEach(function (el) {
      el.classList.toggle('hidden', dark);
    });
  }

  function toggleTheme() {
    var dark = !isDark();
    apply(dark);
    try {
      localStorage.setItem(THEME_KEY, dark ? 'dark' : 'light');
    } catch (e) {
      // Nothing to do — the choice just will not survive the reload.
    }
  }

  /* ------------------------------------------------------------- shell --
   * One definition of the navigation, rendered into
   * <header id="appHeader" data-page="..."> on every page. It is a side panel on
   * a desktop and a bottom bar on a phone — the same DOM either way, restyled by
   * a media query, so a page never has to know which shape it is in.
   *
   * Before this, index linked to /stats and /settings while both of those linked
   * only back to "/", so there was no route from statistics to settings without
   * passing through the search page.
   */
  var ICON = {
    search: '<circle cx="11" cy="11" r="7"></circle><path d="M20 20l-3.5-3.5"></path>',
    trackers: '<rect x="3" y="4" width="18" height="7" rx="2"></rect><rect x="3" y="14" width="18" height="7" rx="2"></rect><path d="M7 7.5h.01M7 17.5h.01"></path>',
    stats: '<path d="M4 20V10M10 20V4M16 20v-7M22 20H2"></path>',
    schedule: '<circle cx="12" cy="13" r="8"></circle><path d="M12 9v4l2.5 1.5M9 2h6"></path>',
    settings: '<circle cx="12" cy="12" r="3"></circle><path d="M19.4 15a1.7 1.7 0 00.3 1.9l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-1.9-.3 1.7 1.7 0 00-1 1.5V21a2 2 0 11-4 0v-.1A1.7 1.7 0 008 19.4a1.7 1.7 0 00-1.9.3l-.1.1a2 2 0 11-2.8-2.8l.1-.1a1.7 1.7 0 00.3-1.9 1.7 1.7 0 00-1.5-1H2a2 2 0 110-4h.1A1.7 1.7 0 004.6 8a1.7 1.7 0 00-.3-1.9l-.1-.1a2 2 0 112.8-2.8l.1.1a1.7 1.7 0 001.9.3H9a1.7 1.7 0 001-1.5V2a2 2 0 114 0v.1a1.7 1.7 0 001 1.5 1.7 1.7 0 001.9-.3l.1-.1a2 2 0 112.8 2.8l-.1.1a1.7 1.7 0 00-.3 1.9V9a1.7 1.7 0 001.5 1H22a2 2 0 110 4h-.1a1.7 1.7 0 00-1.5 1z"></path>'
  };

  var NAV = [
    { group: 'Работа' },
    { id: 'search', href: '/', label: 'Поиск' },
    { id: 'trackers', href: '/trackers', label: 'Трекеры', badge: true },
    { id: 'stats', href: '/stats', label: 'Статистика' },
    { group: 'Система' },
    { id: 'schedule', href: '/schedule', label: 'Расписание' },
    { id: 'settings', href: '/settings', label: 'Настройки' }
  ];

  function svg(path, size) {
    return '<svg width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" ' +
      'stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" ' +
      'aria-hidden="true">' + path + '</svg>';
  }

  var BRAND_ICON = svg('<circle cx="12" cy="12" r="3"></circle>' +
    '<path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M19.1 4.9L17 7M7 17l-2.1 2.1"></path>', 21);

  var MOON_ICON = '<svg data-theme-icon="moon" width="17" height="17" viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"></path></svg>';

  var SUN_ICON = '<svg data-theme-icon="sun" width="17" height="17" viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<circle cx="12" cy="12" r="4"></circle>' +
    '<path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M19.1 4.9l-1.4 1.4M6.3 17.7l-1.4 1.4"></path></svg>';

  function navHTML(page) {
    return NAV.map(function (item) {
      if (item.group) return '<div class="app-nav-title">' + item.group + '</div>';
      var current = item.id === page ? ' aria-current="page"' : '';
      var badge = item.badge ? '<span class="app-badge" data-attention hidden></span>' : '';
      return '<a href="' + item.href + '"' + current + ' title="' + item.label + '">' +
        svg(ICON[item.id], 18) +
        '<span class="app-nav-label">' + item.label + '</span>' + badge + '</a>';
    }).join('');
  }

  function renderHeader() {
    var host = document.getElementById('appHeader');
    if (!host) return;

    host.innerHTML =
      '<div class="app-shell">' +
        '<a class="app-brand" href="/" aria-label="jacred-go — на главную">' +
          '<span class="app-brand-mark">' + BRAND_ICON + '</span>' +
          '<span class="app-brand-text">' +
            '<span class="app-brand-name">jacred-go</span>' +
            '<span class="app-brand-version" data-version></span>' +
          '</span>' +
        '</a>' +
        '<nav class="app-nav" aria-label="Разделы">' + navHTML(host.getAttribute('data-page') || '') + '</nav>' +
        '<div class="app-spacer"></div>' +
        '<div class="app-foot">' +
          '<span class="app-dot" data-health></span>' +
          '<span class="app-foot-text">' +
            '<span class="app-foot-line" data-health-text>Проверка…</span>' +
            '<span class="app-foot-sub" data-lastdb></span>' +
          '</span>' +
          '<button type="button" class="app-icon-btn" data-action="toggle-theme" ' +
            'aria-label="Сменить тему" title="Сменить тему">' + MOON_ICON + SUN_ICON + '</button>' +
        '</div>' +
      '</div>';

    document.documentElement.classList.add('has-shell');

    host.addEventListener('click', function (ev) {
      if (ev.target.closest('[data-action="toggle-theme"]')) toggleTheme();
    });
  }

  /* Everything below is best-effort: the shell must render and navigate whether
   * or not these answer, so each failure is swallowed and simply leaves its slot
   * as it was. */
  function fillOne(url, apply) {
    fetch(url, { headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(apply)
      .catch(function () { });
  }

  function set(sel, text) {
    var el = document.querySelector('#appHeader [' + sel + ']');
    if (el) el.textContent = text;
  }

  function fillStatus() {
    fillOne('/version', function (d) { set('data-version', d.version || ''); });
    fillOne('/lastupdatedb', function (d) {
      if (d.lastupdatedb) set('data-lastdb', 'база: ' + d.lastupdatedb);
    });
    fillOne('/health', function (d) {
      var ok = String(d.status || '').toUpperCase() === 'OK';
      var dot = document.querySelector('#appHeader [data-health]');
      if (dot) dot.classList.toggle('app-dot--down', !ok);
      set('data-health-text', ok ? 'Служба работает' : 'Служба не отвечает');
    });
    // The one number worth carrying onto every page: how many parsers need a
    // human. Without it a dead tracker stays invisible until someone opens the
    // trackers page or the log.
    fillOne('/stats/parsers', function (d) {
      var n = 0;
      (d.trackers || []).forEach(function (t) {
        if (t.disabled) return;
        var r = (t.runs || [])[0];
        if (!r) return;
        if (r.status === 'work_login' || r.status === 'cf-challenge' ||
            r.status === 'error' || r.error || r.http >= 400 || r.failed > 0) n++;
      });
      var badge = document.querySelector('#appHeader [data-attention]');
      if (!badge) return;
      badge.textContent = n;
      badge.hidden = n === 0;
      badge.title = n + ' трекер(ов) требуют внимания';
    });
  }

  function init() {
    renderHeader();
    apply(isDark());
    fillStatus();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.JacredGo = {
    toggleTheme: toggleTheme,
    isDark: isDark,
    prefersDark: prefersDark,
    storedTheme: stored
  };
})();
