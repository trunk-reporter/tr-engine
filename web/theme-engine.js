/**
 * THEME ENGINE — Event Horizon Design System
 * ═══════════════════════════════════════════════════════════════
 * Shared library that applies themes from THEME_CONFIG.
 * Expects theme-config.js to be loaded first.
 *
 * USAGE:
 *   <script src="/theme-config.js"></script>
 *   <script src="/theme-engine.js"></script>
 *
 * That's it. The engine will:
 *   1. Apply CSS variables from the saved theme (or default)
 *   2. Set data-* attributes for feature flags
 *   3. Build the floating theme switcher (if #themeSwitcher exists)
 *   4. Show the theme label toast (if #themeLabel exists)
 *   5. Persist the choice to localStorage
 *
 * Pages that want the switcher UI need these two elements:
 *   <div class="theme-switcher" id="themeSwitcher"></div>
 *   <div class="theme-label" id="themeLabel"></div>
 *
 * Pages that DON'T want the switcher can omit them entirely.
 * The theme still applies from localStorage — it just won't
 * show the picker or the toast.
 *
 * PUBLIC API (available as window.ThemeEngine):
 *   ThemeEngine.apply('obsidian')    — switch theme
 *   ThemeEngine.current()            — get current theme key
 *   ThemeEngine.list()               — get array of theme keys
 *   ThemeEngine.config               — raw THEME_CONFIG reference
 *   ThemeEngine.default              — the fallback theme key
 *
 * CONFIGURATION:
 *   Set window.THEME_ENGINE_OPTIONS before loading this script:
 *   window.THEME_ENGINE_OPTIONS = {
 *     default: 'appleGlass',    // fallback theme if nothing saved
 *     storageKey: 'eh-theme',   // localStorage key
 *     switcher: true,           // build theme picker (default: true)
 *     toast: true,              // show theme label on switch (default: true)
 *     toastDuration: 2000,      // ms to show toast (default: 2000)
 *     header: true,             // inject sticky site header (default: true)
 *     pageTitle: '',            // override card-title meta tag
 *     pageSubtitle: '',         // override card-description meta tag
 *     homeHref: 'index.html',   // home link target
 *   };
 * ═══════════════════════════════════════════════════════════════
 */

(function() {
  'use strict';

  // ── Options ──
  const opts = Object.assign({
    default: 'appleGlass',
    storageKey: 'eh-theme',
    switcher: true,
    toast: true,
    toastDuration: 2000,
    header: true,
    pageTitle: '',
    pageSubtitle: '',
    homeHref: 'index.html',
  }, window.THEME_ENGINE_OPTIONS || {});

  // ── Guards ──
  if (typeof THEME_CONFIG === 'undefined') {
    console.error('[ThemeEngine] THEME_CONFIG not found. Load theme-config.js before theme-engine.js');
    return;
  }

  const root = document.documentElement;
  let currentTheme = null;
  let labelTimeout = null;

  // ── Feature flag → data attribute mapping ──
  const FEATURE_MAP = {
    scanlines:      'scanlines',
    glowText:       'glow-text',
    squareElements: 'square-elements',
    gradientLogo:   'gradient-logo',
    invertedLabels: 'inverted-labels',
  };

  /**
   * Apply a theme by key.
   * This is the core function — everything flows from here.
   */
  function applyTheme(key) {
    const theme = THEME_CONFIG[key];
    if (!theme) {
      console.warn(`[ThemeEngine] Theme "${key}" not found`);
      return false;
    }

    currentTheme = key;

    // 1. Set all CSS custom properties
    for (const [prop, val] of Object.entries(theme.vars)) {
      root.style.setProperty(`--${prop}`, val);
    }

    // 2. Set feature flag data attributes on <body>
    for (const [flag, attr] of Object.entries(FEATURE_MAP)) {
      document.body.setAttribute(`data-${attr}`, String(theme.features[flag] || false));
    }

    // 3. Update switcher active state (header picker + any legacy floating switcher)
    document.querySelectorAll('.theme-btn').forEach(btn =>
      btn.classList.toggle('active', btn.dataset.theme === key)
    );

    // 4. Persist to localStorage
    try { localStorage.setItem(opts.storageKey, key); } catch (e) {}

    // 5. Show toast (if element exists)
    if (opts.toast) showLabel(theme.name);

    // 6. Dispatch event for other scripts to react
    window.dispatchEvent(new CustomEvent('themechange', {
      detail: { key, name: theme.name, theme }
    }));

    return true;
  }

  /**
   * Build the theme switcher buttons inside a given container element.
   */
  function buildSwitcher(container) {
    if (!container || !opts.switcher) return;
    container.innerHTML = '';
    for (const [key, theme] of Object.entries(THEME_CONFIG)) {
      const btn = document.createElement('button');
      btn.className = 'theme-btn';
      btn.dataset.theme = key;
      btn.dataset.name = theme.name;
      btn.style.background = theme.switcher.bg;
      btn.style.boxShadow = `inset 0 -8px 0 ${theme.switcher.accent}`;
      btn.addEventListener('click', () => applyTheme(key));
      container.appendChild(btn);
    }
  }

  /**
   * Inject the sticky site header with nav dropdown and theme picker.
   * Reads page title/subtitle from meta tags or THEME_ENGINE_OPTIONS.
   */
  function injectHeader() {
    // Read title/subtitle — options win, fall back to meta tags
    const metaTitle = (document.querySelector('meta[name="card-title"]') || {}).content || '';
    const metaDesc  = (document.querySelector('meta[name="card-description"]') || {}).content || '';
    const pageTitle    = opts.pageTitle    || metaTitle    || 'Event Horizon';
    const pageSubtitle = opts.pageSubtitle || metaDesc     || 'tr-engine';
    const homeHref     = opts.homeHref     || 'index.html';

    // Inject CSS
    const style = document.createElement('style');
    style.id = 'eh-header-styles';
    style.textContent = `
/* ── Event Horizon injected header ── */
.eh-header {
  position: sticky; top: 0; z-index: 200;
  background: color-mix(in srgb, var(--bg) 92%, transparent);
  backdrop-filter: blur(var(--glass-blur, 20px));
  -webkit-backdrop-filter: blur(var(--glass-blur, 20px));
  border-bottom: 1px solid var(--glass-border);
  padding: 0 20px;
  display: flex; align-items: center; gap: 16px;
  height: 60px;
  transition: background 0.5s, border-color 0.4s;
}

/* Mark */
.eh-header-mark {
  width: 42px; height: 42px; flex-shrink: 0;
  background: var(--mark-bg, var(--accent));
  box-shadow: var(--mark-shadow, 0 0 10px var(--accent-glow));
  border-radius: var(--radius-sm);
  display: flex; align-items: center; justify-content: center;
  position: relative; overflow: hidden;
  transition: background 0.5s, border-radius 0.4s, box-shadow 0.5s;
  text-decoration: none;
}
.eh-header-mark::before {
  content: ''; position: absolute; top: 0; left: -5%; right: -5%; height: 52%;
  background: linear-gradient(180deg, rgba(255,255,255,0.3) 0%, transparent 100%);
  border-radius: 0 0 50% 50%;
}
.eh-header-mark svg { width: 20px; height: 20px; position: relative; z-index: 1; }
[data-square-elements="true"] .eh-header-mark { border-radius: 0; }

/* Nav dropdown */
.eh-nav-wrap { position: relative; }
.eh-nav-btn {
  display: flex; align-items: center; gap: 5px;
  background: none; border: none; cursor: pointer; padding: 4px 0;
  color: var(--text); transition: opacity 0.15s;
}
.eh-nav-btn:hover { opacity: 0.7; }
.eh-nav-title {
  font-family: var(--font-display);
  font-weight: var(--font-weight-display);
  font-size: 20px; letter-spacing: -0.02em; line-height: 1;
  color: var(--accent);
  transition: color 0.4s, font-family 0.3s, text-shadow 0.4s;
}
[data-glow-text="true"] .eh-nav-title {
  text-shadow: 0 0 16px var(--accent-glow);
}
[data-gradient-logo="true"] .eh-nav-title {
  background: linear-gradient(135deg, #3d7aed, #8b5cf6, #ec4899, #f97316);
  -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text;
}
.eh-nav-chevron {
  color: var(--text-muted);
  transition: transform 0.25s, color 0.15s;
  flex-shrink: 0;
}
.eh-nav-wrap.open .eh-nav-chevron { transform: rotate(180deg); color: var(--accent); }

.eh-nav-dropdown {
  position: absolute; top: calc(100% + 10px); left: 0;
  min-width: 280px; max-width: 360px;
  max-height: calc(100vh - 80px); overflow-y: auto;
  /* background: var(--glass-bg); border: 1px solid var(--glass-border); */
  background: color-mix(in srgb, var(--bg) 90%, transparent);
  border-radius: var(--radius-sm);
  backdrop-filter: blur(72px); -webkit-backdrop-filter: blur(72px);
  box-shadow: 0 4px 24px rgba(0,0,0,0.18);
  padding: 6px; z-index: 1000;
  opacity: 0; transform: translateY(-6px) scale(0.97);
  pointer-events: none; transition: opacity 0.2s, transform 0.2s;
}
.eh-nav-wrap.open .eh-nav-dropdown {
  opacity: 1; transform: translateY(0) scale(1); pointer-events: auto;
}
[data-square-elements="true"] .eh-nav-dropdown { border-radius: 0; }

.eh-nav-link {
  display: flex; align-items: center; gap: 10px; padding: 9px 10px;
  border-radius: var(--radius-xs);
  background: transparent; border: 1px solid transparent;
  text-decoration: none; color: var(--text-mid);
  transition: background 0.15s, border-color 0.15s, color 0.15s;
}
.eh-nav-link:hover {
  background: var(--tile-bg); border-color: var(--tile-border); color: var(--text);
}
[data-square-elements="true"] .eh-nav-link { border-radius: 0; }
.eh-nav-link-icon {
  width: 26px; height: 26px; border-radius: 7px; flex-shrink: 0;
  background: var(--tile-bg); border: 1px solid var(--tile-border);
  display: flex; align-items: center; justify-content: center;
  transition: all 0.15s;
}
[data-square-elements="true"] .eh-nav-link-icon { border-radius: 0; }
.eh-nav-link-icon svg { width: 12px; height: 12px; stroke: var(--text-muted); transition: stroke 0.15s; }
.eh-nav-link:hover .eh-nav-link-icon svg { stroke: var(--accent); }
.eh-nav-link-text { flex: 1; min-width: 0; }
.eh-nav-link-title { font-family: var(--font-body); font-size: 12.5px; font-weight: 600; color: inherit; }
.eh-nav-link-desc { font-family: var(--font-mono); font-size: 9px; color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-top: 2px; }
.eh-nav-link-arrow { font-size: 15px; font-weight: 300; color: var(--text-faint); transition: transform 0.15s, color 0.15s; }
.eh-nav-link:hover .eh-nav-link-arrow { transform: translateX(3px); color: var(--accent); }
.eh-nav-placeholder { font-family: var(--font-mono); font-size: 11px; color: var(--text-muted); padding: 12px 10px; text-align: center; }

/* Page visibility edit mode */
.eh-nav-manage {
  display: flex; align-items: center; justify-content: center; gap: 6px;
  padding: 8px 10px 4px; margin-top: 4px;
  border-top: 1px solid color-mix(in srgb, var(--border), transparent 60%);
  cursor: pointer; background: none; border-left: none; border-right: none; border-bottom: none;
  font-family: var(--font-mono); font-size: 9.5px; font-weight: 600;
  color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.06em;
  transition: color 0.15s; width: 100%;
}
.eh-nav-manage:hover { color: var(--accent); }
.eh-nav-manage svg { width: 12px; height: 12px; }
.eh-nav-link.eh-hidden { opacity: 0.35; }
.eh-nav-link.eh-hidden:hover { opacity: 0.55; }
.eh-nav-vis-toggle {
  width: 22px; height: 22px; flex-shrink: 0;
  display: none; align-items: center; justify-content: center;
  background: none; border: none; cursor: pointer; padding: 0;
  color: var(--text-muted); border-radius: 4px; transition: color 0.15s, background 0.15s;
}
.eh-nav-vis-toggle:hover { color: var(--accent); background: var(--tile-bg); }
.eh-nav-vis-toggle svg { width: 14px; height: 14px; }
.eh-nav-dropdown.eh-editing .eh-nav-vis-toggle { display: flex; }
.eh-nav-dropdown.eh-editing .eh-nav-link-arrow { display: none; }

/* Subtitle */
.eh-header-sub {
  font-family: var(--font-mono); font-size: 11px; letter-spacing: 0.08em;
  color: var(--text-muted); margin-left: 2px;
  transition: color 0.4s;
}

/* Right side */
.eh-header-right {
  margin-left: auto;
  display: flex; align-items: center; gap: 8px;
}

/* Picker (inside header) */
.eh-picker {
  display: flex; gap: 4px; padding: 4px;
  background: var(--tile-bg);
  border: 1px solid var(--border);
  border-radius: 10px;
  transition: all 0.5s;
}
[data-square-elements="true"] .eh-picker { border-radius: 0; }
.eh-picker .theme-btn {
  width: 22px; height: 22px; border-radius: 5px;
  border: 2px solid transparent;
  cursor: pointer; transition: all 0.25s;
  position: relative; overflow: visible; font-size: 0; flex-shrink: 0;
}
.eh-picker .theme-btn:hover { transform: scale(1.15); }
.eh-picker .theme-btn.active {
  border-color: var(--accent); box-shadow: 0 0 8px var(--accent-glow); transform: scale(1.1);
}
[data-square-elements="true"] .eh-picker .theme-btn { border-radius: 0; }
.eh-picker .theme-btn::after {
  content: attr(data-name);
  position: absolute; bottom: -24px; left: 50%; transform: translateX(-50%);
  font-size: 8px; font-family: var(--font-mono);
  letter-spacing: 1px; text-transform: uppercase;
  white-space: nowrap; color: var(--text-muted);
  background: var(--bg); border: 1px solid var(--border);
  padding: 2px 6px; border-radius: 4px;
  opacity: 0; transition: opacity 0.2s; pointer-events: none;
  z-index: 10001;
}
.eh-picker .theme-btn:hover::after { opacity: 1; }

/* Update badge */
.eh-update-badge {
  display: inline-flex; align-items: center; gap: 5px;
  font-family: var(--font-mono); font-size: 10px; font-weight: 600;
  color: var(--bg); background: var(--accent);
  padding: 3px 10px; border-radius: 10px;
  text-decoration: none; white-space: nowrap;
  transition: opacity 0.15s, background 0.3s;
}
.eh-update-badge:hover { opacity: 0.85; }
[data-square-elements="true"] .eh-update-badge { border-radius: 0; }

/* Network status dot */
.eh-net-dot {
  width: 8px; height: 8px; border-radius: 50%;
  display: none; flex-shrink: 0;
}
.eh-net-dot.connected { display: block; background: var(--success); }
.eh-net-dot.connecting { display: block; background: var(--warning, #f59e0b); animation: eh-pulse 1.5s infinite; }
.eh-net-dot.disconnected { display: block; background: var(--danger); }
@keyframes eh-pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.4; } }

/* ── Mobile ── */
@media (max-width: 600px) {
  .eh-header { height: 50px; padding: 0 12px; gap: 10px; }
  .eh-header-mark { width: 32px; height: 32px; }
  .eh-header-mark svg { width: 16px; height: 16px; }
  .eh-nav-title { font-size: 16px; }
  .eh-header-sub { display: none; }
  .eh-picker { overflow-x: auto; -webkit-overflow-scrolling: touch; scrollbar-width: none; }
  .eh-picker::-webkit-scrollbar { display: none; }
  .eh-picker .theme-btn { width: 18px; height: 18px; }
  .eh-nav-dropdown { max-width: calc(100vw - 24px); min-width: unset; }
}
    `;
    document.head.appendChild(style);

    // Build header HTML
    const NAV_ICON = `<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 3a3 3 0 0 0-3 3v12a3 3 0 0 0 3 3 3 3 0 0 0 3-3 3 3 0 0 0-3-3H6a3 3 0 0 0-3 3 3 3 0 0 0 3 3 3 3 0 0 0 3-3V6a3 3 0 0 0-3-3 3 3 0 0 0-3 3 3 3 0 0 0 3 3h12a3 3 0 0 0 3-3 3 3 0 0 0-3-3z"/></svg>`;
    const MARK_ICON = `<svg viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2.2" stroke-linecap="round"><circle cx="12" cy="12" r="3"/><path d="M12 1v4M12 19v4M4.22 4.22l2.83 2.83M16.95 16.95l2.83 2.83M1 12h4M19 12h4M4.22 19.78l2.83-2.83M16.95 7.05l2.83-2.83"/></svg>`;

    const header = document.createElement('header');
    header.className = 'eh-header';
    header.id = 'eh-header';
    header.innerHTML = `
      <a class="eh-header-mark" href="${homeHref}" title="Event Horizon">${MARK_ICON}</a>
      <div class="eh-nav-wrap" id="eh-nav-wrap">
        <button class="eh-nav-btn" id="eh-nav-btn" aria-haspopup="true" aria-expanded="false">
          <span class="eh-nav-title">${pageTitle}</span>
          <svg class="eh-nav-chevron" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
        </button>
        <div class="eh-nav-dropdown" id="eh-nav-dropdown" role="menu">
          <div class="eh-nav-placeholder">loading…</div>
        </div>
      </div>
      <span class="eh-header-sub">${pageSubtitle}</span>
      <div class="eh-header-right">
        <span class="eh-net-dot" id="eh-net-dot" title="Disconnected"></span>
        <div class="eh-picker" id="eh-picker"></div>
      </div>
    `;

    document.body.insertBefore(header, document.body.firstChild);

    // Wire up nav toggle
    const wrap = document.getElementById('eh-nav-wrap');
    const btn  = document.getElementById('eh-nav-btn');
    btn.addEventListener('click', () => {
      const isOpen = wrap.classList.toggle('open');
      btn.setAttribute('aria-expanded', isOpen);
      if (isOpen) loadNavPages();
    });
    document.addEventListener('click', e => {
      if (!wrap.contains(e.target)) {
        wrap.classList.remove('open');
        if (_navEditing) { _navEditing = false; renderNavPages(); }
      }
    });
    document.addEventListener('keydown', e => {
      if (e.key === 'Escape') {
        wrap.classList.remove('open');
        if (_navEditing) { _navEditing = false; renderNavPages(); }
      }
    });
  }

  /**
   * Fetch pages from the API and populate the nav dropdown.
   * Supports edit mode for hiding/showing pages via localStorage.
   */
  const HIDDEN_PAGES_KEY = 'eh-hidden-pages';
  let _navPages = null;  // cached page list from API
  let _navEditing = false;

  function getHiddenPages() {
    try { return JSON.parse(localStorage.getItem(HIDDEN_PAGES_KEY)) || []; }
    catch { return []; }
  }
  function setHiddenPages(arr) {
    localStorage.setItem(HIDDEN_PAGES_KEY, JSON.stringify(arr));
  }

  function loadNavPages() {
    if (_navPages) { renderNavPages(); return; }

    fetch('/api/v1/pages')
      .then(r => r.json())
      .then(pages => { _navPages = pages; renderNavPages(); })
      .catch(() => {
        const dropdown = document.getElementById('eh-nav-dropdown');
        dropdown.textContent = '';
        const ph = document.createElement('div');
        ph.className = 'eh-nav-placeholder';
        ph.textContent = 'could not load pages';
        dropdown.appendChild(ph);
      });
  }

  function renderNavPages() {
    const dropdown = document.getElementById('eh-nav-dropdown');
    const currentFile = window.location.pathname.split('/').pop() || 'index.html';
    const hidden = getHiddenPages();

    const others = (_navPages || []).filter(p => {
      const file = (p.path || '').split('/').pop();
      return file !== currentFile;
    });

    if (!others.length) {
      dropdown.textContent = '';
      const ph = document.createElement('div');
      ph.className = 'eh-nav-placeholder';
      ph.textContent = 'no other pages';
      dropdown.appendChild(ph);
      return;
    }

    const visible = _navEditing ? others : others.filter(p => !hidden.includes(p.path));

    const NAV_SVG_ATTRS = { viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round' };

    function makeSvg(attrs, paths) {
      const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      for (const [k, v] of Object.entries({ ...NAV_SVG_ATTRS, ...attrs })) svg.setAttribute(k, v);
      paths.forEach(d => {
        if (typeof d === 'string') {
          const p = document.createElementNS('http://www.w3.org/2000/svg', 'path'); p.setAttribute('d', d); svg.appendChild(p);
        } else {
          const el = document.createElementNS('http://www.w3.org/2000/svg', d.tag);
          for (const [k, v] of Object.entries(d)) { if (k !== 'tag') el.setAttribute(k, v); }
          svg.appendChild(el);
        }
      });
      return svg;
    }

    function navIcon() {
      return makeSvg({}, [
        { tag: 'rect', x: '3', y: '3', width: '7', height: '7', rx: '1' },
        { tag: 'rect', x: '14', y: '3', width: '7', height: '7', rx: '1' },
        { tag: 'rect', x: '3', y: '14', width: '7', height: '7', rx: '1' },
        { tag: 'rect', x: '14', y: '14', width: '7', height: '7', rx: '1' },
      ]);
    }
    function eyeOpen() {
      return makeSvg({}, [
        'M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z',
        { tag: 'circle', cx: '12', cy: '12', r: '3' },
      ]);
    }
    function eyeClosed() {
      return makeSvg({}, [
        'M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24',
        { tag: 'line', x1: '1', y1: '1', x2: '23', y2: '23' },
      ]);
    }
    function pencilIcon() {
      return makeSvg({}, [
        'M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7',
        'M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z',
      ]);
    }
    function checkIcon() {
      return makeSvg({}, [{ tag: 'polyline', points: '20 6 9 17 4 12' }]);
    }

    dropdown.classList.toggle('eh-editing', _navEditing);
    dropdown.textContent = '';

    if (visible.length) {
      visible.forEach(p => {
        const isHidden = hidden.includes(p.path);

        const link = document.createElement('a');
        link.href = _navEditing ? 'javascript:void(0)' : p.path;
        link.className = 'eh-nav-link' + (isHidden ? ' eh-hidden' : '');
        link.setAttribute('role', 'menuitem');
        link.dataset.pagePath = p.path;

        const iconWrap = document.createElement('div');
        iconWrap.className = 'eh-nav-link-icon';
        iconWrap.appendChild(navIcon());
        link.appendChild(iconWrap);

        const textWrap = document.createElement('div');
        textWrap.className = 'eh-nav-link-text';
        const titleEl = document.createElement('div');
        titleEl.className = 'eh-nav-link-title';
        titleEl.textContent = p.title;
        textWrap.appendChild(titleEl);
        if (p.description) {
          const descEl = document.createElement('div');
          descEl.className = 'eh-nav-link-desc';
          descEl.textContent = p.description;
          textWrap.appendChild(descEl);
        }
        link.appendChild(textWrap);

        const visBtn = document.createElement('button');
        visBtn.className = 'eh-nav-vis-toggle';
        visBtn.dataset.path = p.path;
        visBtn.title = (isHidden ? 'Show' : 'Hide') + ' page';
        visBtn.appendChild(isHidden ? eyeClosed() : eyeOpen());
        link.appendChild(visBtn);

        const arrow = document.createElement('span');
        arrow.className = 'eh-nav-link-arrow';
        arrow.textContent = '\u203a';
        link.appendChild(arrow);

        dropdown.appendChild(link);

        // Wire eye toggle
        if (_navEditing) {
          visBtn.addEventListener('click', (e) => {
            e.preventDefault();
            e.stopPropagation();
            const h = getHiddenPages();
            const idx = h.indexOf(p.path);
            if (idx >= 0) h.splice(idx, 1); else h.push(p.path);
            setHiddenPages(h);
            renderNavPages();
          });
          link.addEventListener('click', (e) => e.preventDefault());
        }
      });
    } else {
      const ph = document.createElement('div');
      ph.className = 'eh-nav-placeholder';
      ph.textContent = 'all pages hidden';
      dropdown.appendChild(ph);
    }

    // Manage / Done button
    const manageBtn = document.createElement('button');
    manageBtn.className = 'eh-nav-manage';
    manageBtn.id = 'eh-nav-manage';
    manageBtn.appendChild(_navEditing ? checkIcon() : pencilIcon());
    const manageLabel = document.createElement('span');
    manageLabel.textContent = _navEditing ? 'Done' : 'Manage pages';
    manageBtn.appendChild(manageLabel);
    dropdown.appendChild(manageBtn);

    manageBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      _navEditing = !_navEditing;
      renderNavPages();
    });
  }

  /**
   * Show the floating theme name toast.
   */
  function showLabel(name) {
    const el = document.getElementById('themeLabel');
    if (!el) return;

    el.textContent = name;
    el.classList.remove('show');
    void el.offsetWidth; // trigger reflow for re-animation
    el.classList.add('show');

    clearTimeout(labelTimeout);
    labelTimeout = setTimeout(() => el.classList.remove('show'), opts.toastDuration);
  }

  /**
   * Read saved theme from localStorage, with fallback.
   */
  function getSavedTheme() {
    try {
      const saved = localStorage.getItem(opts.storageKey);
      if (saved && THEME_CONFIG[saved]) return saved;
    } catch (e) {}
    return opts.default;
  }

  // ── Keyboard shortcut: Ctrl/Cmd + Shift + T cycles themes ──
  document.addEventListener('keydown', function(e) {
    if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key === 'T') {
      e.preventDefault();
      const keys = Object.keys(THEME_CONFIG);
      const idx = keys.indexOf(currentTheme);
      const next = keys[(idx + 1) % keys.length];
      applyTheme(next);
    }
  });

  // ── Initialize ──
  // Wait for DOM if needed, but apply theme vars immediately
  // to prevent flash of unstyled content
  const initialTheme = getSavedTheme();

  // Apply vars to :root immediately (before DOM ready)
  const theme = THEME_CONFIG[initialTheme];
  if (theme) {
    for (const [prop, val] of Object.entries(theme.vars)) {
      root.style.setProperty(`--${prop}`, val);
    }
  }

  // Build UI and fully apply once DOM is ready
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function() {
      if (opts.header !== false) injectHeader();
      buildSwitcher(document.getElementById('eh-picker'));
      applyTheme(initialTheme);
    });
  } else {
    if (opts.header !== false) injectHeader();
    buildSwitcher(document.getElementById('eh-picker'));
    applyTheme(initialTheme);
  }

  // ── Update badge ──
  function checkForUpdate() {
    fetch('/api/v1/health')
      .then(r => r.json())
      .then(d => {
        if (!d.update_available) return;
        const right = document.querySelector('.eh-header-right');
        if (!right || right.querySelector('.eh-update-badge')) return;
        const badge = document.createElement('a');
        badge.className = 'eh-update-badge';
        badge.href = d.release_url || '#';
        badge.target = '_blank';
        badge.rel = 'noopener';
        badge.textContent = 'Update: ' + (d.latest_version || 'new');
        right.insertBefore(badge, right.firstChild);
      })
      .catch(() => {});
  }
  if (opts.header !== false) {
    // Check after a short delay to avoid blocking page load
    setTimeout(checkForUpdate, 2000);
  }

  // ── Public API ──
  window.ThemeEngine = {
    apply: applyTheme,
    current: function() { return currentTheme; },
    list: function() { return Object.keys(THEME_CONFIG); },
    config: THEME_CONFIG,
    default: opts.default,
  };

  window.trTheme = { invalidateNavCache: function() { _navPages = null; } };

  // ── Network status dot API ──
  window.ehSetNetworkStatus = function(state) {
    var dot = document.getElementById('eh-net-dot');
    if (!dot) return;
    dot.className = 'eh-net-dot ' + state;
    dot.title = state === 'connected' ? 'Connected' :
                state === 'connecting' ? 'Connecting\u2026' : 'Disconnected';
  };

})();

// bfcache fix — re-apply theme when browser restores page from back/forward cache
window.addEventListener('pageshow', function(event) {
  if (event.persisted) {
    try {
      const saved = localStorage.getItem('eh-theme');
      if (saved && window.ThemeEngine) ThemeEngine.apply(saved);
    } catch (e) {}
  }
});
