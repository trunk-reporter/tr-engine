/**
 * auth.js — Centralized auth for tr-engine web pages.
 * Include via <script src="auth.js?v=2"></script> before page scripts.
 *
 * Auto-detects legacy (read+write tokens) vs JWT mode from /api/v1/auth-init.
 * Patches fetch/EventSource to inject tokens, intercept 401/403, prompt for
 * credentials, and retry transparently. Exposes window.trAuth public API.
 */
(function () {
  'use strict';

  // ── Constants ────────────────────────────────────────────────────
  var KEY_READ  = 'tr-engine-token';
  var KEY_WRITE = 'tr-engine-write-token';
  var KEY_JWT   = 'tr-engine-jwt';

  // ── State ────────────────────────────────────────────────────────
  var mode       = 'none';   // 'legacy' | 'jwt' | 'none'
  var readToken  = '';
  var writeToken = localStorage.getItem(KEY_WRITE) || '';
  var jwtToken   = localStorage.getItem(KEY_JWT) || '';
  var jwtRole    = '';
  var pendingPrompt = null;  // shared Promise for debounced prompts

  // ── Mode detection (synchronous XHR) ─────────────────────────────
  try {
    var xhr = new XMLHttpRequest();
    xhr.open('GET', '/api/v1/auth-init', false);
    if (jwtToken) xhr.setRequestHeader('Authorization', 'Bearer ' + jwtToken);
    xhr.send();
    if (xhr.status === 200) {
      var data = JSON.parse(xhr.responseText);

      if (data.mode) {
        // New auth-init response (v0.10+)
        if (data.mode === 'open') {
          mode = 'none';
          // No auth needed — clear any stale tokens
          readToken = '';
        } else if (data.mode === 'token') {
          // User must provide token — check localStorage, lazy-prompt on 401
          readToken = localStorage.getItem(KEY_READ) || '';
          mode = readToken ? 'legacy' : 'none';
        } else if (data.mode === 'full') {
          // JWT login available
          if (data.read_token) {
            readToken = data.read_token;
            localStorage.setItem(KEY_READ, readToken);
          }
          if (data.jwt_enabled && jwtToken) {
            mode = 'jwt';
          } else {
            mode = readToken ? 'legacy' : 'none';
            if (jwtToken) {
              jwtToken = '';
              localStorage.removeItem(KEY_JWT);
            }
          }
        }
      } else {
        // Legacy auth-init response (pre-v0.10) — backward compat
        readToken = data.token || '';
        if (readToken) localStorage.setItem(KEY_READ, readToken);
        if (data.user && jwtToken) {
          mode = 'jwt';
          jwtRole = data.user.role || '';
        } else {
          mode = readToken ? 'legacy' : 'none';
          if (jwtToken) {
            jwtToken = '';
            localStorage.removeItem(KEY_JWT);
          }
        }
      }
    }
  } catch (e) {
    // Server unreachable or auth-init not available
    readToken = localStorage.getItem(KEY_READ) || '';
    mode = readToken ? 'legacy' : 'none';
  }

  // Backward compat: populate global used by old inline scripts
  if (readToken) window.__TR_AUTH_TOKEN__ = readToken;

  // ── Helpers ──────────────────────────────────────────────────────
  function isLocalAPI(url) {
    if (url.startsWith('/api/')) return true;
    try {
      var u = new URL(url, location.origin);
      return u.origin === location.origin && u.pathname.startsWith('/api/');
    } catch (e) {
      return false;
    }
  }

  function isMutation(method) {
    var m = (method || 'GET').toUpperCase();
    return m === 'POST' || m === 'PUT' || m === 'PATCH' || m === 'DELETE';
  }

  function effectiveToken(method) {
    if (mode === 'jwt') return jwtToken;
    if (mode === 'legacy' && isMutation(method) && writeToken) return writeToken;
    return readToken;
  }

  function effectiveReadToken() {
    if (mode === 'jwt') return jwtToken;
    return readToken;
  }

  // Clone fetch arguments so we can retry after auth prompt.
  // Request bodies are one-shot streams, so we must clone before first send.
  function cloneArgs(input, init) {
    if (input instanceof Request) {
      return { input: input.clone(), init: init };
    }
    return { input: input, init: init ? Object.assign({}, init) : {} };
  }

  // ── Fetch patch ──────────────────────────────────────────────────
  var _fetch = window.fetch;

  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input
      : input instanceof Request ? input.url : '';
    var method = (init && init.method) ? init.method
      : (input instanceof Request ? input.method : 'GET');
    var isAPI = isLocalAPI(url);

    // Clone for potential retry
    var saved = isAPI ? cloneArgs(input, init) : null;

    // Inject auth header
    if (isAPI) {
      init = init || {};
      var headers = new Headers(init.headers || {});
      if (!headers.has('Authorization')) {
        var tok = effectiveToken(method);
        if (tok) headers.set('Authorization', 'Bearer ' + tok);
      }
      init.headers = headers;
    }

    return _fetch.call(this, input, init).then(function (resp) {
      if (!isAPI) return resp;

      // 401 — auth required
      if (resp.status === 401) {
        return handleAuthError('token', saved, method);
      }

      // 403 on mutation — write access needed
      if (resp.status === 403 && isMutation(method)) {
        if (mode === 'jwt') {
          return handleAuthError('insufficient', saved, method);
        }
        return handleAuthError('write-token', saved, method);
      }

      return resp;
    });
  };

  // ── EventSource patch ────────────────────────────────────────────
  var _EventSource = window.EventSource;

  window.EventSource = function (url, opts) {
    var tok = effectiveReadToken();
    if (tok && isLocalAPI(url)) {
      var sep = url.indexOf('?') >= 0 ? '&' : '?';
      url = url + sep + 'token=' + encodeURIComponent(tok);
    }
    return new _EventSource(url, opts);
  };
  window.EventSource.prototype  = _EventSource.prototype;
  window.EventSource.CONNECTING = _EventSource.CONNECTING;
  window.EventSource.OPEN       = _EventSource.OPEN;
  window.EventSource.CLOSED     = _EventSource.CLOSED;

  // ── Auth error handler (debounced) ───────────────────────────────
  function handleAuthError(promptType, saved, method) {
    if (!pendingPrompt) {
      pendingPrompt = showModal(promptType).then(function (result) {
        pendingPrompt = null;
        return result;
      }, function (err) {
        pendingPrompt = null;
        throw err;
      });
    }

    return pendingPrompt.then(function (result) {
      if (!result) {
        // User cancelled — return a synthetic error response so the page's
        // .catch() or error handling fires. Do NOT re-fire the fetch, as
        // the patched fetch would intercept the 401/403 again.
        return new Response(JSON.stringify({ error: 'authentication cancelled' }), {
          status: 401, headers: { 'Content-Type': 'application/json' }
        });
      }
      // Retry with new credentials
      var retryInit = saved.init || {};
      var headers = new Headers(retryInit.headers || {});
      headers.set('Authorization', 'Bearer ' + effectiveToken(method));
      retryInit.headers = headers;
      return _fetch.call(window, saved.input, retryInit);
    });
  }

  // ── Silent JWT refresh ───────────────────────────────────────────
  function tryRefresh() {
    return _fetch.call(window, '/api/v1/auth/refresh', {
      method: 'POST',
      credentials: 'same-origin'
    }).then(function (resp) {
      if (!resp.ok) return false;
      return resp.json().then(function (data) {
        if (data.access_token) {
          jwtToken = data.access_token;
          localStorage.setItem(KEY_JWT, jwtToken);
          if (data.user) jwtRole = data.user.role || '';
          return true;
        }
        return false;
      });
    }).catch(function () { return false; });
  }

  // ── Modal UI ─────────────────────────────────────────────────────
  function showModal(promptType) {
    // In JWT mode with existing token, try silent refresh first
    if (promptType === 'token' && mode === 'jwt' && jwtToken) {
      return tryRefresh().then(function (ok) {
        if (ok) return true;
        // Refresh failed — clear JWT, show login
        jwtToken = '';
        localStorage.removeItem(KEY_JWT);
        mode = readToken ? 'legacy' : 'none';
        return showModalDOM('login');
      });
    }

    // Map prompt types for JWT mode
    if (promptType === 'token' && mode !== 'legacy') {
      promptType = 'login';
    }

    return showModalDOM(promptType);
  }

  function showModalDOM(promptType) {
    return new Promise(function (resolve) {
      var overlay = document.createElement('div');
      overlay.id = 'tr-auth-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.7);z-index:99999;display:flex;align-items:center;justify-content:center;font-family:system-ui,sans-serif';
      var box = document.createElement('div');
      box.style.cssText = 'background:#1a1a2e;border:1px solid #333;border-radius:8px;padding:24px;max-width:360px;width:90%;color:#e0e0e0';
      var title = document.createElement('h2');
      title.style.cssText = 'margin:0 0 8px;font-size:16px;color:#fff';
      var desc = document.createElement('p');
      desc.style.cssText = 'margin:0 0 16px;font-size:13px;color:#999;line-height:1.4';
      var errMsg = document.createElement('p');
      errMsg.style.cssText = 'margin:0 0 12px;font-size:13px;color:#f44;line-height:1.4;display:none';

      function dismiss(result) {
        if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
        resolve(result);
      }
      function showErr(msg) { errMsg.textContent = msg; errMsg.style.display = 'block'; }
      function mount(els, focusEl) {
        box.appendChild(title); box.appendChild(desc);
        for (var i = 0; i < els.length; i++) box.appendChild(els[i]);
        overlay.appendChild(box); document.body.appendChild(overlay);
        if (focusEl) focusEl.focus();
      }
      overlay.addEventListener('click', function (e) { if (e.target === overlay) dismiss(false); });

      // ── Insufficient permissions (JWT) ──
      if (promptType === 'insufficient') {
        title.textContent = 'Insufficient Permissions';
        desc.textContent = 'Your account does not have permission to perform this action. Contact an administrator for access.';
        var okBtn = makeButton('OK', true);
        okBtn.onclick = function () { dismiss(false); };
        mount([okBtn], okBtn);
        return;
      }

      // ── Token prompt (legacy read) ──
      if (promptType === 'token') {
        title.textContent = 'Authentication Required';
        desc.textContent = 'This instance requires an API token. Enter it below \u2014 it will be saved in your browser.';
        var input = makeInput('password', 'Bearer token');
        input.value = readToken;
        var btnRow = makeButtonRow(makeButton('Connect', true), makeButton('Cancel', false));
        btnRow.children[0].onclick = function () {
          var val = input.value.trim();
          if (val) { readToken = val; localStorage.setItem(KEY_READ, readToken); window.__TR_AUTH_TOKEN__ = readToken; }
          dismiss(!!val);
        };
        btnRow.children[1].onclick = function () { dismiss(false); };
        input.onkeydown = function (e) { if (e.key === 'Enter') btnRow.children[0].onclick(); };
        mount([errMsg, input, btnRow], input);
        return;
      }

      // ── Write token prompt (legacy) ──
      if (promptType === 'write-token') {
        title.textContent = 'Write Access Required';
        desc.textContent = 'This action requires a write token. Enter it below to continue.';
        var wtInput = makeInput('password', 'Write token');
        wtInput.value = writeToken;
        var wtRow = makeButtonRow(makeButton('Save & Retry', true), makeButton('Cancel', false));
        wtRow.children[0].onclick = function () {
          var val = wtInput.value.trim();
          if (val) { writeToken = val; localStorage.setItem(KEY_WRITE, val); }
          dismiss(!!val);
        };
        wtRow.children[1].onclick = function () { dismiss(false); };
        wtInput.onkeydown = function (e) { if (e.key === 'Enter') wtRow.children[0].onclick(); };
        mount([errMsg, wtInput, wtRow], wtInput);
        return;
      }

      // ── Login (JWT) ──
      if (promptType === 'login') {
        title.textContent = 'Log In';
        desc.textContent = 'Enter your credentials to continue.';
        var userInput = makeInput('text', 'Username');
        var passInput = makeInput('password', 'Password');
        passInput.style.marginTop = '8px';
        var loginRow = makeButtonRow(makeButton('Log In', true), makeButton('Cancel', false));
        var submitBtn = loginRow.children[0];
        loginRow.children[1].onclick = function () { dismiss(false); };
        function doLogin() {
          var u = userInput.value.trim(), p = passInput.value;
          if (!u || !p) return;
          submitBtn.disabled = true; submitBtn.textContent = 'Logging in\u2026';
          _fetch.call(window, '/api/v1/auth/login', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ username: u, password: p }), credentials: 'same-origin'
          }).then(function (resp) {
            if (!resp.ok) {
              submitBtn.disabled = false; submitBtn.textContent = 'Log In';
              showErr(resp.status === 401 ? 'Invalid username or password.'
                : resp.status === 429 ? 'Too many attempts. Try again later.'
                : 'Login failed (' + resp.status + ')');
              return;
            }
            return resp.json().then(function (data) {
              if (data.access_token) {
                jwtToken = data.access_token; localStorage.setItem(KEY_JWT, jwtToken);
                mode = 'jwt'; if (data.user) jwtRole = data.user.role || '';
                dismiss(true);
              }
            });
          }).catch(function () {
            submitBtn.disabled = false; submitBtn.textContent = 'Log In';
            showErr('Network error. Check your connection.');
          });
        }
        submitBtn.onclick = doLogin;
        passInput.onkeydown = function (e) { if (e.key === 'Enter') doLogin(); };
        userInput.onkeydown = function (e) { if (e.key === 'Enter') passInput.focus(); };
        mount([errMsg, userInput, passInput, loginRow], userInput);
        return;
      }
    });
  }

  // ── DOM helpers (XSS-safe: textContent only) ─────────────────────
  function makeInput(type, placeholder) {
    var el = document.createElement('input');
    el.type = type;
    el.placeholder = placeholder;
    el.style.cssText = 'width:100%;box-sizing:border-box;padding:8px 12px;background:#0d0d1a;border:1px solid #444;border-radius:4px;color:#fff;font-size:14px';
    return el;
  }

  function makeButton(label, primary) {
    var btn = document.createElement('button');
    btn.textContent = label;
    btn.style.cssText = primary
      ? 'flex:1;padding:8px;background:#4a6cf7;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:14px'
      : 'padding:8px 12px;background:transparent;color:#999;border:1px solid #444;border-radius:4px;cursor:pointer;font-size:13px';
    return btn;
  }

  function makeButtonRow() {
    var row = document.createElement('div');
    row.style.cssText = 'display:flex;gap:8px;margin-top:12px';
    for (var i = 0; i < arguments.length; i++) row.appendChild(arguments[i]);
    return row;
  }

  // ── Public API ───────────────────────────────────────────────────
  window.trAuth = {
    getToken: function () { return effectiveReadToken(); },
    getWriteToken: function () {
      if (mode === 'jwt') return jwtToken;
      return writeToken || readToken;
    },
    setToken: function (t) {
      readToken = t || '';
      if (readToken) localStorage.setItem(KEY_READ, readToken);
      else localStorage.removeItem(KEY_READ);
    },
    getMode: function () { return mode; },
    hasWriteAccess: function () {
      if (mode === 'jwt') return jwtRole === 'editor' || jwtRole === 'admin';
      return !!writeToken;
    },
    showPrompt: function () { return showModal('token'); },
    logout: function () {
      jwtToken = '';
      jwtRole = '';
      localStorage.removeItem(KEY_JWT);
      _fetch.call(window, '/api/v1/auth/logout', {
        method: 'POST',
        credentials: 'same-origin'
      }).finally(function () {
        mode = readToken ? 'legacy' : 'none';
        location.reload();
      });
    }
  };
})();
