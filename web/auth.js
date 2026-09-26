/**
 * auth.js — API-key auth for the tr-engine demo pages.
 * Include via <script src="auth.js?v=3"></script> before page scripts.
 *
 * The engine authenticates client software with API keys. This file keeps one
 * key per browser in localStorage['tr-engine-api-key'] and sends it as
 * "Authorization: Bearer <key>" on same-origin /api/ requests. Without a key,
 * requests carry no credential and the engine's anonymous access policy
 * decides what they may read.
 *
 * Browsers can't attach headers to EventSource, <audio> or WebSocket, so for
 * those it mints short-lived, listen-only tickets (POST /api/v1/tickets) and
 * puts them in ?ticket=. Keys and tickets are only ever sent to this page's
 * own origin; pages pointed at another engine with ?api= get plain requests.
 *
 * Exposes window.trAuth (see the bottom of this file).
 */
(function () {
  'use strict';

  // ── Constants ────────────────────────────────────────────────────
  var STORAGE_KEY = 'tr-engine-api-key';
  var OLD_STORAGE_KEYS = ['tr-engine-token', 'tr-engine-write-token', 'tr-engine-jwt'];
  var API = '/api/v1';

  var WHOAMI_TIMEOUT_MS = 10000;
  var TICKET_REFRESH_MS = 5 * 60 * 1000;  // refresh the cached ticket below this
  var TICKET_MIN_MS = 60 * 1000;          // never hand out a ticket with less left
  var TICKET_RETRY_MS = 30 * 1000;        // background refresh retry after a failure
  var BACKOFF_MIN_MS = 1000;
  var BACKOFF_MAX_MS = 30000;

  var ES_CONNECTING = 0, ES_OPEN = 1, ES_CLOSED = 2;

  var _fetch = window.fetch;
  var NativeEventSource = window.EventSource;

  // ── Storage (every access guarded: storage can be blocked) ───────
  function storageGet(k) {
    try { return window.localStorage.getItem(k); } catch (e) { return null; }
  }
  function storageSet(k, v) {
    try { window.localStorage.setItem(k, v); return true; } catch (e) { return false; }
  }
  function storageRemove(k) {
    try { window.localStorage.removeItem(k); } catch (e) { /* storage blocked */ }
  }

  // Credentials of the old auth modes are never used again.
  for (var i = 0; i < OLD_STORAGE_KEYS.length; i++) storageRemove(OLD_STORAGE_KEYS[i]);

  // ── State ────────────────────────────────────────────────────────
  var apiKey = (storageGet(STORAGE_KEY) || '').trim();
  var whoamiData = null;      // last 200 body of GET /whoami for the current credential
  var keyStatus = apiKey ? 'unknown' : 'none';  // 'none' | 'valid' | 'invalid' | 'unknown'
  var anonymous = null;       // {access, restricted} from the last whoami
  var whoamiPromise = null;
  var readyEntry = null;      // {key, promise}
  var ticket = null;          // {key, value, expiresAt, ttl}
  var ticketFlight = null;    // {key, promise}
  var ticketTimer = null;
  var modal = null;           // {kind, promise}
  var streams = new Set();    // live EventSource wrappers
  var warned = {};

  // ── Helpers ──────────────────────────────────────────────────────
  function warnOnce(id, msg) {
    if (warned[id]) return;
    warned[id] = true;
    if (window.console && console.warn) console.warn(msg);
  }

  // True for URLs on this page's own origin under /api/. ws:/wss: URLs count
  // when they point at this host with the matching scheme.
  function isLocalAPI(url) {
    if (url == null) return false;
    var u;
    try { u = new URL(String(url), location.href); } catch (e) { return false; }
    var sameOrigin = u.origin === location.origin ||
      (u.host === location.host &&
        ((u.protocol === 'ws:' && location.protocol === 'http:') ||
         (u.protocol === 'wss:' && location.protocol === 'https:')));
    return sameOrigin && u.pathname.indexOf('/api/') === 0;
  }

  function isRequest(input) {
    return typeof Request === 'function' && input instanceof Request;
  }

  function requestURL(input) {
    if (typeof input === 'string') return input;
    if (isRequest(input)) return input.url;
    if (input && typeof input.href === 'string') return input.href;  // URL object
    return input == null ? '' : String(input);
  }

  function isMutation(method) {
    var m = String(method || 'GET').toUpperCase();
    return m !== 'GET' && m !== 'HEAD' && m !== 'OPTIONS';
  }

  function readJSON(resp) {
    return resp.json().catch(function () { return null; });
  }

  function errorCode(body) {
    return body && typeof body.code === 'string' ? body.code : '';
  }

  function errorMessage(body) {
    return body && typeof body.error === 'string' ? body.error : '';
  }

  function authError(code, message, status) {
    var err = new Error(message || code);
    err.code = code;
    err.status = status || 0;
    return err;
  }

  // Replace (or drop, when the value is empty) query parameters without
  // re-encoding the rest of the URL.
  function withParams(url, params) {
    var s = String(url);
    var hashAt = s.indexOf('#');
    var hash = hashAt >= 0 ? s.slice(hashAt) : '';
    var base = hashAt >= 0 ? s.slice(0, hashAt) : s;
    var qAt = base.indexOf('?');
    var path = qAt >= 0 ? base.slice(0, qAt) : base;
    var query = qAt >= 0 ? base.slice(qAt + 1) : '';
    var names = Object.keys(params);
    var parts = query.split('&').filter(function (p) {
      if (!p) return false;
      var name = p.split('=')[0];
      try { name = decodeURIComponent(name.replace(/\+/g, ' ')); } catch (e) { /* keep raw */ }
      return names.indexOf(name) < 0;
    });
    names.forEach(function (n) {
      if (params[n] != null && params[n] !== '') parts.push(n + '=' + encodeURIComponent(params[n]));
    });
    return path + (parts.length ? '?' + parts.join('&') : '') + hash;
  }

  var SCOPE_IMPLIES = { admin: ['admin', 'edit', 'listen'], edit: ['edit', 'listen'], listen: ['listen'], upload: ['upload'] };

  function effectiveScopes() {
    var out = [];
    var list = whoamiData && Array.isArray(whoamiData.scopes) ? whoamiData.scopes : [];
    list.forEach(function (s) {
      (SCOPE_IMPLIES[s] || [s]).forEach(function (x) { if (out.indexOf(x) < 0) out.push(x); });
    });
    return out;
  }

  function hasScope(scope) {
    return effectiveScopes().indexOf(String(scope)) >= 0;
  }

  // Escape a value for use in HTML text or a quoted attribute. Shared by the
  // demo pages for every API-provided string they build HTML from.
  function escapeHTML(value) {
    if (value == null) return '';
    return String(value)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // ── whoami ───────────────────────────────────────────────────────
  function fetchWhoami(key) {
    var ctrl = typeof AbortController === 'function' ? new AbortController() : null;
    var timer = ctrl ? setTimeout(function () { ctrl.abort(); }, WHOAMI_TIMEOUT_MS) : null;
    var headers = {};
    if (key) headers.Authorization = 'Bearer ' + key;
    return _fetch.call(window, API + '/whoami', {
      headers: headers, cache: 'no-store', signal: ctrl ? ctrl.signal : undefined
    }).then(function (resp) {
      return readJSON(resp).then(function (body) { return { status: resp.status, body: body }; });
    }).catch(function () {
      return { status: 0, body: null };
    }).then(function (r) {
      if (timer) clearTimeout(timer);
      return r;
    });
  }

  function loadWhoami() {
    var key = apiKey;
    var p = fetchWhoami(key).then(function (r) {
      if (key !== apiKey) return whoamiPromise !== p ? whoamiPromise : null;  // superseded
      if (r.status === 200 && r.body) {
        whoamiData = r.body;
        anonymous = r.body.anonymous || null;
        keyStatus = key ? 'valid' : 'none';
        return whoamiData;
      }
      whoamiData = null;
      if (r.status === 401 && key && errorCode(r.body) === 'invalid_key') {
        keyStatus = 'invalid';
        // Learn the anonymous policy, so the key prompt can offer to go on without a key.
        return fetchWhoami('').then(function (a) {
          if (key === apiKey && a.status === 200 && a.body) anonymous = a.body.anonymous || null;
          return null;
        });
      }
      keyStatus = key ? 'unknown' : 'none';
      if (r.status === 404) warnOnce('whoami404', 'tr-engine: GET /api/v1/whoami not found; this engine predates API keys');
      return null;
    });
    whoamiPromise = p;
    return p;
  }

  function whoamiReady() {
    return whoamiPromise || loadWhoami();
  }

  // The anonymous access policy ('off' | 'listen'), refreshing whoami once if unknown.
  function anonymousAccess() {
    if (anonymous && anonymous.access) return Promise.resolve(anonymous.access);
    return loadWhoami().then(function () { return anonymous ? anonymous.access : ''; });
  }

  // A stored key that may be able to mint tickets. Unknown counts as yes; the engine decides.
  function keyCanListen() {
    if (!apiKey || keyStatus === 'invalid') return false;
    if (keyStatus === 'valid' && whoamiData) return hasScope('listen');
    return true;
  }

  // ── Tickets ──────────────────────────────────────────────────────
  function refreshThreshold(t) {
    return Math.min(TICKET_REFRESH_MS, t.ttl / 2);
  }

  function cachedTicket(minRemainingMs) {
    if (!ticket || ticket.key !== apiKey) return null;
    return ticket.expiresAt - Date.now() >= minRemainingMs ? ticket : null;
  }

  function mintTicket() {
    var key = apiKey;
    if (!key) return Promise.reject(authError('key_required', 'no API key stored'));
    if (ticketFlight && ticketFlight.key === key) return ticketFlight.promise;
    var promise = _fetch.call(window, API + '/tickets', {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json' },
      body: '{}',
      cache: 'no-store'
    }).then(function (resp) {
      return readJSON(resp).then(function (body) {
        if (!resp.ok || !body || typeof body.ticket !== 'string') {
          var code = errorCode(body) || 'http_' + resp.status;
          if (code === 'invalid_key' && key === apiKey) keyStatus = 'invalid';
          throw authError(code, errorMessage(body) || 'ticket request failed (HTTP ' + resp.status + ')', resp.status);
        }
        // Measure the lifetime against the server's clock, then apply it to ours,
        // so a skewed client clock doesn't make tickets look fresh or stale.
        var now = Date.now();
        var ttl = 600000;
        var exp = Date.parse(body.expires_at);
        var served = Date.parse(resp.headers.get('Date') || '');
        if (isFinite(exp)) ttl = isFinite(served) ? exp - served : exp - now;
        ttl = Math.max(1000, Math.min(ttl, 3600 * 1000));
        if (key === apiKey) {
          ticket = { key: key, value: body.ticket, expiresAt: now + ttl, ttl: ttl };
          scheduleTicketRefresh();
        }
        return body.ticket;
      });
    }, function () {
      throw authError('network', 'could not reach tr-engine to mint a ticket');
    });
    var entry = { key: key, promise: promise };
    ticketFlight = entry;
    function done() { if (ticketFlight === entry) ticketFlight = null; }
    promise.then(done, done);
    return promise;
  }

  function getTicket(minRemainingMs) {
    var t = cachedTicket(minRemainingMs);
    return t ? Promise.resolve(t.value) : mintTicket();
  }

  function dropTicket(value) {
    if (ticket && ticket.value === value) ticket = null;
  }

  function scheduleTicketRefresh(delay) {
    clearTimeout(ticketTimer);
    ticketTimer = null;
    if (delay == null) {
      if (!ticket) return;
      delay = ticket.expiresAt - refreshThreshold(ticket) - Date.now();
    }
    ticketTimer = setTimeout(refreshTicketIfNeeded, Math.max(1000, delay));
  }

  // Keep a ticket with at least 5 minutes left while the page is visible.
  // Background tabs skip timer refreshes; visibilitychange catches up.
  function refreshTicketIfNeeded() {
    if (!keyCanListen()) return;
    if (document.visibilityState === 'hidden') return;
    var t = ticket && ticket.key === apiKey ? ticket : null;
    if (t && t.expiresAt - Date.now() >= refreshThreshold(t)) { scheduleTicketRefresh(); return; }
    mintTicket().catch(backgroundTicketFailed);
  }

  function isTransient(err) {
    var status = (err && err.status) || 0;
    return (err && err.code === 'network') || status === 408 || status === 429 || status >= 500;
  }

  function backgroundTicketFailed(err) {
    if (isTransient(err)) scheduleTicketRefresh(TICKET_RETRY_MS);
    // invalid_key / insufficient_scope: the next request or stream prompts.
  }

  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') refreshTicketIfNeeded();
  });

  // Synchronous: a same-origin API URL with the cached ticket appended, or
  // the URL unchanged. Never returns a ticket with less than 60 s left; in
  // that case it starts a refresh and returns the plain URL.
  function mediaUrl(url) {
    if (url == null || url === '') return url;
    var s = String(url);
    if (!isLocalAPI(s) || !keyCanListen()) return s;
    var t = cachedTicket(TICKET_MIN_MS);
    if (!t) {
      mintTicket().catch(backgroundTicketFailed);
      return s;
    }
    if (t.expiresAt - Date.now() < refreshThreshold(t)) mintTicket().catch(backgroundTicketFailed);
    return withParams(s, { ticket: t.value });
  }

  // Async: a same-origin API URL with a ticket that has at least 5 minutes
  // left (or a newly minted one with {fresh: true}). Cross-origin URLs, and
  // anything without a usable key, resolve to the URL unchanged.
  function ticketUrl(url, opts) {
    var s = String(url);
    if (!isLocalAPI(s) || !apiKey) return Promise.resolve(s);
    var fresh = !!(opts && opts.fresh);
    return whoamiReady().then(function () {
      if (!keyCanListen()) return s;
      return (fresh ? mintTicket() : getTicket(TICKET_REFRESH_MS)).then(function (t) {
        return withParams(s, { ticket: t });
      }, function (err) {
        if (err && err.code === 'invalid_key') {
          return promptForKey('invalid', true).then(function (ok) {
            if (!ok || !keyCanListen()) return s;
            return getTicket(TICKET_REFRESH_MS).then(function (t) {
              return withParams(s, { ticket: t });
            }, function () { return s; });
          });
        }
        return s;
      });
    });
  }

  // ── Key changes ──────────────────────────────────────────────────
  function applyKeyChange(newKey, knownWhoami) {
    newKey = String(newKey || '').trim();
    if (newKey === apiKey && !knownWhoami) return whoamiReady();
    apiKey = newKey;
    ticket = null;
    ticketFlight = null;
    clearTimeout(ticketTimer);
    ticketTimer = null;
    readyEntry = null;
    if (knownWhoami) {
      whoamiData = knownWhoami;
      anonymous = knownWhoami.anonymous || anonymous;
      keyStatus = apiKey ? 'valid' : 'none';
      whoamiPromise = Promise.resolve(knownWhoami);
    } else {
      whoamiData = null;
      keyStatus = apiKey ? 'unknown' : 'none';
      loadWhoami();
    }
    streams.forEach(function (s) { s._credentialChanged(); });
    try {
      window.dispatchEvent(new CustomEvent('trauth:change', { detail: { hasKey: !!apiKey } }));
    } catch (e) { /* old browser */ }
    return ready();
  }

  function setKey(k, knownWhoami) {
    var v = String(k == null ? '' : k).trim();
    if (!v) return clearKey();
    if (!storageSet(STORAGE_KEY, v)) warnOnce('storage', 'tr-engine: browser storage is blocked; the API key is kept for this page only');
    return applyKeyChange(v, knownWhoami);
  }

  function clearKey() {
    storageRemove(STORAGE_KEY);
    return applyKeyChange('');
  }

  // Keep tabs in step when another tab sets or forgets the key.
  window.addEventListener('storage', function (e) {
    if (e.key === STORAGE_KEY || e.key === null) applyKeyChange(storageGet(STORAGE_KEY) || '');
  });

  // Resolves after whoami and, when a key is stored, after the first ticket.
  function ready() {
    if (!readyEntry || readyEntry.key !== apiKey) {
      var key = apiKey;
      var p = whoamiReady().then(function () {
        if (key && key === apiKey && keyCanListen()) {
          return getTicket(TICKET_REFRESH_MS).catch(backgroundTicketFailed);
        }
      }).then(function () { return whoamiData; });
      readyEntry = { key: key, promise: p };
    }
    return readyEntry.promise;
  }

  // ── fetch patch ──────────────────────────────────────────────────
  function withAuth(init, headers, key) {
    var out = {};
    if (init) for (var k in init) out[k] = init[k];
    var h = new Headers(headers);
    h.delete('Authorization');
    if (key) h.set('Authorization', 'Bearer ' + key);
    out.headers = h;
    return out;
  }

  window.fetch = function (input, init) {
    var url = requestURL(input);
    if (!isLocalAPI(url)) return _fetch.apply(window, arguments);

    var method = String((init && init.method) || (isRequest(input) ? input.method : 'GET')).toUpperCase();
    // Explicit init headers replace a Request's own headers, as in fetch() itself.
    var baseHeaders = (init && init.headers !== undefined) ? init.headers
      : (isRequest(input) ? input.headers : undefined);
    var headers;
    try { headers = new Headers(baseHeaders || {}); } catch (e) { return _fetch.apply(window, arguments); }
    // A caller that sets its own Authorization (admin.html, Swagger UI) manages
    // its credential itself: no key injection, no prompts.
    if (headers.has('Authorization')) return _fetch.apply(window, arguments);

    var retryInput = input;
    if (isRequest(input)) {
      try { retryInput = input.clone(); } catch (e) { retryInput = null; }  // body already used
    }

    return whoamiReady().then(function () {
      var sentKey = apiKey;
      return _fetch.call(window, input, withAuth(init, headers, sentKey)).then(function (resp) {
        return afterResponse(resp, {
          method: method, sentKey: sentKey, retryInput: retryInput, init: init, headers: headers
        });
      });
    });
  };

  function retryRequest(ctx) {
    return _fetch.call(window, ctx.retryInput, withAuth(ctx.init, ctx.headers, apiKey));
  }

  function promptThenRetry(reason, resp, ctx) {
    // 'switch' follows the user's own click, so it is never suppressed.
    return promptForKey(reason, reason !== 'switch').then(function (ok) {
      return ok && ctx.retryInput ? retryRequest(ctx) : resp;
    });
  }

  function afterResponse(resp, ctx) {
    if (resp.status !== 401 && resp.status !== 403) return resp;
    return readJSON(resp.clone()).then(function (body) {
      var code = errorCode(body);
      if (resp.status === 401) {
        if (code === 'invalid_key') {
          if (apiKey && apiKey !== ctx.sentKey && ctx.retryInput) return retryRequest(ctx);  // replaced meanwhile
          if (ctx.sentKey) keyStatus = 'invalid';
          return promptThenRetry('invalid', resp, ctx);
        }
        if (code === 'key_required' && !ctx.sentKey) {
          if (apiKey && ctx.retryInput) return retryRequest(ctx);  // a key was entered meanwhile
          return anonymousAccess().then(function (access) {
            if (access === 'off' && !apiKey) return promptThenRetry('required', resp, ctx);
            return resp;
          });
        }
        return resp;
      }
      // 403: only mutations get an explanation; reads pass through untouched.
      if (isMutation(ctx.method) && code === 'insufficient_scope') {
        return explainScope(errorMessage(body)).then(function (choice) {
          if (choice !== 'switch') return resp;
          return promptThenRetry('switch', resp, ctx);
        });
      }
      return resp;
    });
  }

  // ── EventSource replacement ──────────────────────────────────────
  // Same-origin /api/ streams get a wrapper that mirrors the EventSource API.
  // With a stored key it connects with a fresh ticket and reconnects itself
  // (new ticket + last_event_id, with backoff); it also handles the engine's
  // "event: auth" close signals. Without a key it connects with the URL as
  // given and leaves reconnecting to the browser, taking over only when the
  // browser gives up (e.g. a 429) while anonymous listening is allowed.
  // When the key is set, replaced or forgotten, open streams reconnect with
  // the new credential. Cross-origin URLs get the native EventSource.
  var TrEventSource = null;

  if (typeof NativeEventSource === 'function' && typeof EventTarget === 'function') {
    TrEventSource = class TrEventSource extends EventTarget {
      constructor(url, opts) {
        super();
        this._rawUrl = String(url);
        this._opts = opts;
        this._state = ES_CONNECTING;
        this._inner = null;
        this._conn = 0;
        this._mode = '';
        this._usedTicket = '';
        this._lastEventId = '';
        this._attempt = 0;
        this._timer = null;
        this._pageClosed = false;
        this._types = new Set(['message', 'auth']);
        this._handlers = { open: null, message: null, error: null };
        var self = this;
        ['open', 'message', 'error'].forEach(function (type) {
          EventTarget.prototype.addEventListener.call(self, type, function (e) {
            var h = self._handlers[type];
            if (typeof h === 'function') h.call(self, e);
          });
        });
        streams.add(this);
        this._connect();
      }

      get url() {
        try { return new URL(this._rawUrl, location.href).href; } catch (e) { return this._rawUrl; }
      }
      get withCredentials() { return !!(this._opts && this._opts.withCredentials); }
      get readyState() { return this._state; }
      get onopen() { return this._handlers.open; }
      set onopen(fn) { this._handlers.open = typeof fn === 'function' ? fn : null; }
      get onmessage() { return this._handlers.message; }
      set onmessage(fn) { this._handlers.message = typeof fn === 'function' ? fn : null; }
      get onerror() { return this._handlers.error; }
      set onerror(fn) { this._handlers.error = typeof fn === 'function' ? fn : null; }

      addEventListener(type, listener, options) {
        super.addEventListener(type, listener, options);
        type = String(type);
        if (type === 'open' || type === 'error' || this._types.has(type)) return;
        this._types.add(type);
        if (this._inner && this._forward) this._inner.addEventListener(type, this._forward);
      }

      close() {
        this._pageClosed = true;
        this._teardown();
        this._state = ES_CLOSED;
        streams.delete(this);
      }

      _teardown() {
        clearTimeout(this._timer);
        this._timer = null;
        this._conn++;
        if (this._inner) {
          this._inner.onopen = null;
          this._inner.onerror = null;
          try { this._inner.close(); } catch (e) { /* already closed */ }
          this._inner = null;
        }
        this._forward = null;
      }

      _emit(type) {
        this.dispatchEvent(new Event(type));
      }

      _connect() {
        if (this._pageClosed) return;
        this._teardown();
        var conn = this._conn;
        this._state = ES_CONNECTING;
        if (!apiKey) {
          this._mode = 'plain';
          this._open(this._lastEventId ? withParams(this._rawUrl, { last_event_id: this._lastEventId }) : this._rawUrl, conn);
          return;
        }
        this._mode = 'ticket';
        var self = this;
        whoamiReady().then(function () {
          if (keyStatus === 'invalid') throw authError('invalid_key', 'the stored API key was rejected');
          if (!keyCanListen()) throw authError('insufficient_scope', 'this API key cannot listen');
          return getTicket(TICKET_REFRESH_MS);
        }).then(function (t) {
          if (conn !== self._conn || self._pageClosed) return;
          self._usedTicket = t;
          self._open(withParams(self._rawUrl, { ticket: t, last_event_id: self._lastEventId || null }), conn);
        }, function (err) {
          if (conn !== self._conn || self._pageClosed) return;
          self._ticketFailed(err);
        });
      }

      _open(url, conn) {
        var self = this;
        var es = new NativeEventSource(url, this._opts);
        var forward = function (ev) {
          if (conn !== self._conn) return;
          if (ev.lastEventId) self._lastEventId = ev.lastEventId;
          self.dispatchEvent(new MessageEvent(ev.type, {
            data: ev.data, lastEventId: ev.lastEventId || self._lastEventId, origin: ev.origin
          }));
          if (ev.type === 'auth' && conn === self._conn) self._authEvent(ev);
        };
        this._inner = es;
        this._forward = forward;
        this._types.forEach(function (t) { es.addEventListener(t, forward); });
        es.onopen = function () {
          if (conn !== self._conn) return;
          self._state = ES_OPEN;
          self._attempt = 0;
          self._emit('open');
        };
        es.onerror = function () {
          if (conn !== self._conn) return;
          if (self._mode === 'ticket') {
            if (self._state !== ES_OPEN) dropTicket(self._usedTicket);  // failed before opening
            self._teardown();
            self._state = ES_CONNECTING;
            self._emit('error');
            self._scheduleReconnect();
            return;
          }
          if (es.readyState !== ES_CLOSED) {  // the browser is reconnecting on its own
            self._state = ES_CONNECTING;
            self._emit('error');
            return;
          }
          // The browser gave up (HTTP error, 429, wrong content type). Retry
          // with backoff only while anonymous listening is allowed.
          self._teardown();
          var c = self._conn;
          if (anonymous && anonymous.access === 'listen') {
            self._state = ES_CONNECTING;
            self._emit('error');
            loadWhoami().then(function () {
              if (c !== self._conn || self._pageClosed) return;
              if (apiKey || (anonymous && anonymous.access === 'listen')) { self._scheduleReconnect(); return; }
              self._state = ES_CLOSED;
              self._emit('error');
              streamAuthLost('key_required');
            });
          } else {
            // Most likely 401 key_required: ask for a key when anonymous
            // access is off (a new key revives this stream).
            self._state = ES_CLOSED;
            self._emit('error');
            if (!apiKey) streamAuthLost('key_required');
          }
        };
      }

      _scheduleReconnect() {
        this._teardown();
        if (this._pageClosed) return;
        var delay = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * Math.pow(2, this._attempt));
        this._attempt++;
        delay = delay / 2 + Math.random() * delay / 2;
        var self = this;
        clearTimeout(this._timer);
        this._timer = setTimeout(function () { self._connect(); }, delay);
      }

      _ticketFailed(err) {
        var code = (err && err.code) || '';
        if (isTransient(err)) {
          this._state = ES_CONNECTING;
          this._emit('error');
          this._scheduleReconnect();
          return;
        }
        this._state = ES_CLOSED;
        this._emit('error');
        streamAuthLost(code || 'invalid_key');
      }

      _authEvent(ev) {
        var code = '';
        try { code = JSON.parse(ev.data).code || ''; } catch (e) { /* not JSON */ }
        if (code === 'ticket_expired') {
          dropTicket(this._usedTicket);
          this._attempt = 0;
          this._connect();
          return;
        }
        this._teardown();
        this._state = ES_CLOSED;
        this._emit('error');
        streamAuthLost(code || 'invalid_key');
      }

      _credentialChanged() {
        if (this._pageClosed) return;
        this._attempt = 0;
        this._connect();
      }
    };

    var PatchedEventSource = function EventSource(url, opts) {
      if (!new.target) throw new TypeError("Failed to construct 'EventSource': Please use the 'new' operator.");
      if (!isLocalAPI(url)) return new NativeEventSource(url, opts);
      return new TrEventSource(url, opts);
    };
    PatchedEventSource.prototype = TrEventSource.prototype;
    [PatchedEventSource, TrEventSource, TrEventSource.prototype].forEach(function (o) {
      o.CONNECTING = ES_CONNECTING;
      o.OPEN = ES_OPEN;
      o.CLOSED = ES_CLOSED;
    });
    try {
      Object.defineProperty(PatchedEventSource, Symbol.hasInstance, {
        value: function (o) { return o instanceof NativeEventSource || o instanceof TrEventSource; }
      });
    } catch (e) { /* no Symbol.hasInstance */ }
    window.EventSource = PatchedEventSource;
  }

  // Streams (SSE, the audio WebSocket) closed for auth reasons don't reconnect
  // on their own: re-check whoami, then ask for a key or explain.
  function streamAuthLost(code) {
    return loadWhoami().then(function () {
      if (code === 'invalid_key' || keyStatus === 'invalid') return promptForKey('invalid', true);
      if (code === 'key_required') {
        if (!apiKey && anonymous && anonymous.access === 'off') return promptForKey('required', true);
        return false;
      }
      if (code === 'insufficient_scope') {
        return explainScope('This key cannot stream live data; it needs the listen, edit or admin scope.').then(function (choice) {
          return choice === 'switch' ? promptForKey('switch') : false;
        });
      }
      return false;
    });
  }

  // ── Modals (DOM built with textContent only) ─────────────────────
  function openModal(kind, build) {
    if (modal) {
      if (modal.kind === kind) return modal.promise;
      return modal.promise.then(function () { return openModal(kind, build); });
    }
    var entry = { kind: kind };
    entry.promise = whenBody().then(function () {
      return new Promise(build);
    }).then(function (result) {
      if (modal === entry) modal = null;
      return result;
    }, function () {
      if (modal === entry) modal = null;
      return false;
    });
    modal = entry;
    return entry.promise;
  }

  function whenBody() {
    if (document.body) return Promise.resolve();
    return new Promise(function (resolve) {
      document.addEventListener('DOMContentLoaded', function () { resolve(); }, { once: true });
    });
  }

  function el(tag, css, text) {
    var e = document.createElement(tag);
    if (css) e.style.cssText = css;
    if (text != null) e.textContent = text;
    return e;
  }

  function makeButton(label, primary) {
    var btn = el('button', primary
      ? 'flex:1;padding:8px;background:#4a6cf7;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:14px'
      : 'padding:8px 12px;background:transparent;color:#bbb;border:1px solid #444;border-radius:4px;cursor:pointer;font-size:13px', label);
    btn.type = 'button';
    return btn;
  }

  function makeRow() {
    var row = el('div', 'display:flex;gap:8px;margin-top:12px;flex-wrap:wrap');
    for (var i = 0; i < arguments.length; i++) if (arguments[i]) row.appendChild(arguments[i]);
    return row;
  }

  function dialogShell(titleText, onCancel) {
    var overlay = el('div', 'position:fixed;inset:0;background:rgba(0,0,0,.7);z-index:99999;display:flex;align-items:center;justify-content:center;font-family:system-ui,sans-serif');
    overlay.id = 'tr-auth-overlay';
    var box = el('div', 'background:#1a1a2e;border:1px solid #333;border-radius:8px;padding:24px;max-width:420px;width:90%;color:#e0e0e0;box-sizing:border-box');
    box.setAttribute('role', 'dialog');
    box.setAttribute('aria-modal', 'true');
    var title = el('h2', 'margin:0 0 8px;font-size:16px;color:#fff', titleText);
    title.id = 'tr-auth-title';
    box.setAttribute('aria-labelledby', title.id);
    box.appendChild(title);
    overlay.appendChild(box);
    overlay.addEventListener('click', function (e) { if (e.target === overlay) onCancel(); });
    overlay.addEventListener('keydown', function (e) { if (e.key === 'Escape') onCancel(); });
    return { overlay: overlay, box: box };
  }

  function para(text, css) {
    return el('p', 'margin:0 0 12px;font-size:13px;line-height:1.45;' + (css || 'color:#aaa'), text);
  }

  function describeKey() {
    if (!apiKey) return 'No API key is stored in this browser.';
    if (keyStatus === 'invalid') return 'The stored key was rejected.';
    var k = whoamiData && whoamiData.key;
    if (!k) return 'An API key is stored in this browser.';
    var text = 'Current key: ' + (k.name || '(unnamed)') + ' (' + (k.prefix || '?') + ')';
    var scopes = effectiveScopes();
    if (scopes.length) text += ', scopes: ' + scopes.join(', ');
    if (whoamiData.restricted) text += ', restricted to some systems or talkgroups';
    return text + '.';
  }

  // Paste-key modal. Resolves true when the credential changed (a key was
  // stored, or forgotten), false when cancelled. An automatic prompt the user
  // cancelled isn't shown again for the same key and reason (pages poll);
  // trAuth.showKeyPrompt() always shows it.
  var dismissedPrompts = {};

  function promptForKey(reason, auto) {
    var id = reason + '|' + apiKey;
    if (auto && dismissedPrompts[id]) return Promise.resolve(false);
    return openModal('key', buildKeyPrompt.bind(null, reason)).then(function (ok) {
      if (!ok && auto) dismissedPrompts[id] = true;
      return ok;
    });
  }

  function buildKeyPrompt(reason, resolve) {
    var done = false;
    function finish(result) {
      if (done) return;
      done = true;
      if (shell.overlay.parentNode) shell.overlay.parentNode.removeChild(shell.overlay);
      resolve(result);
    }
    var titles = { invalid: 'API key rejected', required: 'API key required', 'switch': 'Use a different API key' };
    var shell = dialogShell(titles[reason] || 'API key', function () { finish(false); });
    var box = shell.box;
    var descs = {
      invalid: 'This tr-engine rejected the stored API key: it is unknown, revoked or expired. Paste a different key.',
      required: 'This tr-engine only answers requests that carry an API key. Paste one below.',
      'switch': 'Paste a key that has the access you need.'
    };
    box.appendChild(para(descs[reason] || 'Paste an API key (tre_…) to use with this tr-engine.'));
    box.appendChild(para(describeKey(), 'color:#ccc'));
    box.appendChild(para('The key is kept in this browser’s local storage and sent only to this engine. Use a listen or edit key here; paste admin keys only into the Admin page.', 'color:#888;font-size:12px'));
    var errLine = para('', 'color:#f66;display:none');
    box.appendChild(errLine);
    var input = el('input', 'width:100%;box-sizing:border-box;padding:8px 12px;background:#0d0d1a;border:1px solid #444;border-radius:4px;color:#fff;font-size:14px;font-family:monospace');
    input.type = 'password';
    input.placeholder = 'tre_…';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.setAttribute('aria-label', 'API key');
    box.appendChild(input);

    var save = makeButton('Save key', true);
    var cancel = makeButton('Cancel', false);
    var forget = null;
    if (apiKey) {
      var anonListen = anonymous && anonymous.access === 'listen';
      forget = makeButton(anonListen ? 'Continue without a key' : 'Forget key', false);
      forget.onclick = function () { clearKey(); finish(true); };
    }
    box.appendChild(makeRow(save, cancel));
    if (forget) box.appendChild(makeRow(forget));

    function showErr(msg) { errLine.textContent = msg; errLine.style.display = 'block'; }

    function submit() {
      var candidate = input.value.trim();
      if (!candidate) { showErr('Paste a key first.'); return; }
      save.disabled = true;
      save.textContent = 'Checking…';
      fetchWhoami(candidate).then(function (r) {
        save.disabled = false;
        save.textContent = 'Save key';
        if (r.status === 200 && r.body) {
          var scopes = Array.isArray(r.body.scopes) ? r.body.scopes : [];
          var listens = scopes.some(function (s) { return s === 'listen' || s === 'edit' || s === 'admin'; });
          if (!listens) { showErr('This key can only upload; use a listen, edit or admin key.'); return; }
          setKey(candidate, r.body);
          finish(true);
          return;
        }
        if (r.status === 401) { showErr('Key rejected' + (errorMessage(r.body) ? ': ' + errorMessage(r.body) : '.')); return; }
        if (r.status === 404) { showErr('This tr-engine doesn’t support API keys (no /api/v1/whoami); upgrade tr-engine.'); return; }
        if (r.status === 0) { showErr('Couldn’t reach tr-engine. Check your connection.'); return; }
        showErr('Couldn’t check the key (HTTP ' + r.status + ').');
      });
    }
    save.onclick = submit;
    cancel.onclick = function () { finish(false); };
    input.onkeydown = function (e) { if (e.key === 'Enter') submit(); };

    document.body.appendChild(shell.overlay);
    input.focus();
  }

  // Explains a 403 insufficient_scope. Resolves 'switch' or false.
  function explainScope(message) {
    return openModal('scope', function (resolve) {
      var done = false;
      function finish(result) {
        if (done) return;
        done = true;
        if (shell.overlay.parentNode) shell.overlay.parentNode.removeChild(shell.overlay);
        resolve(result);
      }
      var shell = dialogShell('Your API key can’t do this', function () { finish(false); });
      shell.box.appendChild(para(message || 'This action needs a key with more access.', 'color:#ccc'));
      shell.box.appendChild(para(describeKey()));
      var other = makeButton('Use a different key', true);
      var ok = makeButton('OK', false);
      other.onclick = function () { finish('switch'); };
      ok.onclick = function () { finish(false); };
      shell.box.appendChild(makeRow(other, ok));
      document.body.appendChild(shell.overlay);
      ok.focus();
    });
  }

  // ── Start ────────────────────────────────────────────────────────
  loadWhoami();
  ready();

  // ── Public API ───────────────────────────────────────────────────
  function deprecated(name, replacement) {
    warnOnce('shim-' + name, 'trAuth.' + name + '() is deprecated' + (replacement ? '; use trAuth.' + replacement + ' instead' : '') + '. See docs/auth.md.');
  }

  window.trAuth = {
    ready: ready,
    getKey: function () { return apiKey; },
    setKey: function (k) { return setKey(k); },
    clearKey: clearKey,
    whoami: function (opts) {
      if (opts && opts.refresh) return loadWhoami().then(function () { return whoamiData; });
      return whoamiReady().then(function () { return whoamiData; });
    },
    hasScope: hasScope,
    showKeyPrompt: function () { return promptForKey('manual'); },
    // For custom long-lived connections closed with an auth signal
    // ('invalid_key' | 'key_required' | 'insufficient_scope'): re-checks
    // whoami and prompts or explains. Resolves true if the key changed.
    handleAuthLoss: streamAuthLost,
    mediaUrl: mediaUrl,
    ticketUrl: ticketUrl,
    isSameOriginAPI: isLocalAPI,
    escapeHTML: escapeHTML,

    // Deprecated shims, kept so pages built from old templates (or saved with
    // POST /pages) keep working.
    getToken: function () { deprecated('getToken', 'mediaUrl(url) or ticketUrl(url)'); return ''; },
    getWriteToken: function () { deprecated('getWriteToken'); return ''; },
    setToken: function (t) { deprecated('setToken', 'setKey(key)'); return setKey(t); },
    hasWriteAccess: function () { deprecated('hasWriteAccess', "hasScope('edit')"); return hasScope('edit'); },
    showPrompt: function () { deprecated('showPrompt', 'showKeyPrompt()'); return promptForKey('manual'); },
    getMode: function () { deprecated('getMode', 'whoami()'); return apiKey ? 'key' : 'anonymous'; },
    logout: function () { deprecated('logout', 'clearKey()'); return clearKey(); }
  };
})();
