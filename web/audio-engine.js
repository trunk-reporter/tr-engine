// Main thread audio coordinator for live radio streaming.
// Manages WebSocket connection, per-TG audio nodes, mixing, and compression.
// Usage: const engine = new AudioEngine(); await engine.start(); engine.subscribe({tgids: [1234]});
// TG keys are composite "systemId:tgid" strings (e.g. "1:9173").

class AudioEngine {
  constructor(wsPath, options) {
    options = options || {};
    this.wsPath = wsPath || '/api/v1/audio/live';
    this.options = {
      reconnectMaxMs: options.reconnectMaxMs || 30000,
    };
    this.ws = null;
    this.audioCtx = null;
    this.masterGain = null;
    this.masterCompressor = null;
    this.tgNodes = new Map(); // "systemId:tgid" -> { worklet, gain, compressor, panner, ... }
    this.reconnectDelay = 1000;
    this.lastSubscription = null;
    this.listeners = {};
    this._intentionalClose = false;
    this._connGen = 0;          // bumps on every connect/stop; stale sockets are ignored
    this._authClosed = false;   // closed by the server for an auth reason (4401/4403)
    this._freshTicket = false;  // the last attempt never opened: mint a new ticket
    this._serverAudioFormat = null; // set by server 'config' message; null = auto-detect
    this._autoPan = true; // auto-distribute channels across stereo field
    this._jitterTracking = new Map();  // key -> jitter entry
    this._transmissionLog = [];        // completed transmissions
    this._maxTransmissions = 100;
    this._transmissionGapMs = 500;     // gap > 500ms = new transmission
    this._maxDeltas = 500;             // circular buffer size
  }

  // Event emitter
  on(event, fn) {
    if (!this.listeners[event]) this.listeners[event] = [];
    this.listeners[event].push(fn);
    return this;
  }

  off(event, fn) {
    if (!this.listeners[event]) return;
    this.listeners[event] = this.listeners[event].filter(function (f) { return f !== fn; });
  }

  emit(event, data) {
    var fns = this.listeners[event] || [];
    for (var i = 0; i < fns.length; i++) {
      fns[i](data);
    }
  }

  async start() {
    this.audioCtx = new AudioContext({ sampleRate: 48000 });

    // Browsers suspend AudioContext until a user gesture triggers resume().
    // Try immediately (works when start() is called from a click handler).
    // If that fails (e.g. auto-resume on page load), install a one-time
    // gesture listener so the first click/tap/keypress anywhere will unstick it.
    if (this.audioCtx.state === 'suspended') {
      try { await this.audioCtx.resume(); } catch (e) { /* ignore */ }
    }
    if (this.audioCtx.state === 'suspended') {
      this._installGestureResume();
    }

    // Also handle the context getting suspended later (e.g. tab backgrounded on mobile)
    var self = this;
    this.audioCtx.addEventListener('statechange', function () {
      if (self.audioCtx && self.audioCtx.state === 'suspended') {
        self._installGestureResume();
      }
    });

    // AudioWorklet requires a secure context (HTTPS or localhost).
    // Fall back to ScriptProcessorNode on insecure origins (plain HTTP).
    if (this.audioCtx.audioWorklet) {
      await this.audioCtx.audioWorklet.addModule('audio-worklet.js');
      this._useWorklet = true;
    } else {
      console.warn('AudioWorklet unavailable (insecure context). Using ScriptProcessor fallback — serve over HTTPS for best performance.');
      this._useWorklet = false;
    }

    // Master chain: compressor -> gain -> destination
    this.masterCompressor = this.audioCtx.createDynamicsCompressor();
    this.masterCompressor.threshold.value = -24;
    this.masterCompressor.knee.value = 12;
    this.masterCompressor.ratio.value = 4;
    this.masterCompressor.attack.value = 0.003;
    this.masterCompressor.release.value = 0.25;

    this.masterGain = this.audioCtx.createGain();
    this.masterCompressor.connect(this.masterGain);
    this.masterGain.connect(this.audioCtx.destination);

    this._loadSettings();
    this._intentionalClose = false;
    this._authClosed = false;
    this._watchKeyChanges();
    this._connect();
  }

  stop() {
    this._intentionalClose = true;
    this._connGen++;
    if (this.ws) {
      this.ws.close(1000);
      this.ws = null;
    }
    var self = this;
    this.tgNodes.forEach(function (nodes, key) {
      self._removeTG(key);
    });
    this.tgNodes.clear();
    if (this.audioCtx) {
      this.audioCtx.close();
      this.audioCtx = null;
    }
  }

  subscribe(filter) {
    this.lastSubscription = filter;
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type: 'subscribe', ...filter }));
    }
  }

  unsubscribe() {
    this.lastSubscription = null;
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type: 'unsubscribe' }));
    }
  }

  // key: composite "systemId:tgid" string
  setVolume(key, value) {
    var nodes = this.tgNodes.get(key);
    if (nodes) nodes.gain.gain.value = value;
    this._saveSetting('vol_' + key, value);
  }

  getVolume(key) {
    var nodes = this.tgNodes.get(key);
    return nodes ? nodes.gain.gain.value : 1.0;
  }

  setMute(key, muted) {
    var nodes = this.tgNodes.get(key);
    if (nodes) {
      nodes.muted = muted;
      nodes.gain.gain.value = muted ? 0 : (this._loadSetting('vol_' + key) ?? 1.0);
      if (!muted && this._autoPan && !nodes._panAssigned) {
        nodes.panner.pan.value = this._panForKey(key);
        nodes._panAssigned = true;
      }
    }
  }

  setMasterVolume(value) {
    if (this.masterGain) this.masterGain.gain.value = value;
    this._saveSetting('master_vol', value);
  }

  getMasterVolume() {
    return this.masterGain ? this.masterGain.gain.value : 1.0;
  }

  setMasterCompressorEnabled(enabled) {
    if (this.masterCompressor) {
      this.masterCompressor.ratio.value = enabled ? 4 : 1;
    }
    this._saveSetting('master_comp', enabled);
  }

  setTGCompressorEnabled(key, enabled) {
    var nodes = this.tgNodes.get(key);
    if (!nodes) return;
    nodes.compressorEnabled = enabled;
    nodes.compressor.ratio.value = enabled ? 3 : 1;
    this._saveSetting('comp_' + key, enabled);
  }

  setPan(key, value) {
    var nodes = this.tgNodes.get(key);
    if (nodes && nodes.panner) nodes.panner.pan.value = Math.max(-1, Math.min(1, value));
    this._saveSetting('pan_' + key, value);
  }

  getPan(key) {
    var nodes = this.tgNodes.get(key);
    return nodes && nodes.panner ? nodes.panner.pan.value : 0;
  }

  setAutoPan(enabled) {
    this._autoPan = enabled;
    this._saveSetting('auto_pan', enabled);
    if (enabled) {
      var self = this;
      this.tgNodes.forEach(function(nodes, key) {
        if (!nodes.muted && !nodes._panAssigned) {
          nodes.panner.pan.value = self._panForKey(key);
          nodes._panAssigned = true;
        }
      });
    }
  }

  getAutoPan() {
    return this._autoPan;
  }

  // Deterministic pan position from composite key — hash the tgid portion
  _panForKey(key) {
    var tgid = parseInt(String(key).split(':').pop()) || 0;
    var h = ((tgid * 2654435761) >>> 0) % 10000;
    return -0.8 + (h / 10000) * 1.6;
  }

  getActiveTGs() {
    var result = [];
    this.tgNodes.forEach(function (nodes, key) {
      var parts = String(key).split(':');
      result.push({
        key: key,
        tgid: parseInt(parts[1]) || parseInt(parts[0]) || 0,
        systemId: parts.length > 1 ? parseInt(parts[0]) : (nodes.systemId || 0),
        volume: nodes.gain.gain.value,
        muted: !!nodes.muted,
        compressorEnabled: nodes.compressorEnabled,
        lastActivity: nodes.lastActivity,
        pan: nodes.panner ? nodes.panner.pan.value : 0,
      });
    });
    return result;
  }

  isConnected() {
    return this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  getJitterStats() {
    var result = {};
    var self = this;
    this._jitterTracking.forEach(function(entry, key) {
      result[key] = {
        client: self._snapshotJitterStats(entry.stats),
        server: self._snapshotJitterStats(entry.serverStats),
        network: self._snapshotJitterStats(entry.networkStats),
        deltas: entry.deltas.slice(),
        activeTransmission: entry.transmission
      };
    });
    return result;
  }

  getTransmissionLog() {
    return this._transmissionLog.slice();
  }

  getBufferStats() {
    var result = {};
    this.tgNodes.forEach(function(nodes, key) {
      if (nodes.bufferStats) {
        result[key] = nodes.bufferStats;
      }
    });
    return result;
  }

  // Request fresh stats from all worklets (async — arrives via buffer_stats event)
  requestBufferStats() {
    this.tgNodes.forEach(function(nodes) {
      if (nodes.worklet && nodes.worklet.port) {
        nodes.worklet.port.postMessage({ type: 'get_stats' });
      }
    });
  }

  // --- Jitter tracking helpers ---

  _newJitterEntry() {
    return {
      prevClientTime: 0,
      prevServerTs: 0,
      stats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
      serverStats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
      networkStats: { count: 0, min: Infinity, max: 0, mean: 0, m2: 0, last: 0 },
      deltas: [],
      deltaIdx: 0,
      transmission: null
    };
  }

  _addJitterSample(stats, value) {
    stats.count++;
    stats.last = value;
    if (stats.count === 1) {
      stats.min = value;
      stats.max = value;
      stats.mean = value;
      stats.m2 = 0;
      return;
    }
    if (value < stats.min) stats.min = value;
    if (value > stats.max) stats.max = value;
    var delta = value - stats.mean;
    stats.mean += delta / stats.count;
    var delta2 = value - stats.mean;
    stats.m2 += delta * delta2;
  }

  _jitterStddev(stats) {
    if (stats.count < 2) return 0;
    return Math.sqrt(stats.m2 / stats.count);
  }

  _resetJitterStats(stats) {
    stats.count = 0;
    stats.min = Infinity;
    stats.max = 0;
    stats.mean = 0;
    stats.m2 = 0;
    stats.last = 0;
  }

  _snapshotJitterStats(stats) {
    return {
      count: stats.count,
      min: stats.count > 0 ? stats.min : 0,
      max: stats.max,
      mean: stats.mean,
      stddev: this._jitterStddev(stats),
      last: stats.last
    };
  }

  // --- Internal ---

  _installGestureResume() {
    if (this._gestureResumeInstalled) return;
    this._gestureResumeInstalled = true;
    var self = this;
    var resume = function () {
      if (self.audioCtx && self.audioCtx.state === 'suspended') {
        self.audioCtx.resume();
      }
      if (!self.audioCtx || self.audioCtx.state !== 'suspended') {
        document.removeEventListener('click', resume, true);
        document.removeEventListener('keydown', resume, true);
        document.removeEventListener('touchstart', resume, true);
        self._gestureResumeInstalled = false;
      }
    };
    document.addEventListener('click', resume, true);
    document.addEventListener('keydown', resume, true);
    document.addEventListener('touchstart', resume, true);
  }

  // When the API key is set, replaced or forgotten (trAuth), reconnect with
  // the new credential; this also revives a stream closed for auth reasons.
  _watchKeyChanges() {
    if (this._keyWatch) return;
    var self = this;
    this._keyWatch = function () {
      if (self._intentionalClose || !self.audioCtx) return;
      self._authClosed = false;
      self._freshTicket = true;
      self.reconnectDelay = 1000;
      self._connect();
    };
    window.addEventListener('trauth:change', this._keyWatch);
  }

  // Connects to wsPath on this page's origin. Browsers can't send headers on
  // a WebSocket, so with a stored API key the URL carries a short-lived ticket
  // (trAuth.ticketUrl). The server closes with 4401 (invalid_key, key_required,
  // ticket_expired) or 4403 (insufficient_scope) when access ends: only
  // ticket_expired reconnects, at once, with a new ticket.
  async _connect() {
    var self = this;
    if (this._intentionalClose) return;
    var gen = ++this._connGen;
    if (this.ws) {
      var old = this.ws;
      this.ws = null;
      try { old.close(1000); } catch (e) { /* already closed */ }
    }

    var path = this.wsPath;
    if (window.trAuth) {
      try {
        path = await window.trAuth.ticketUrl(this.wsPath, { fresh: this._freshTicket });
      } catch (e) {
        path = this.wsPath;
      }
    }
    if (gen !== this._connGen || this._intentionalClose) return;
    this._freshTicket = false;

    var protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var url = protocol + '//' + location.host + path;
    var opened = false;

    this.ws = new WebSocket(url);
    this.ws.binaryType = 'arraybuffer';

    this.ws.onopen = function () {
      if (gen !== self._connGen) return;
      opened = true;
      self.reconnectDelay = 1000;
      self.emit('status', { connected: true });
      if (self.lastSubscription) {
        self.subscribe(self.lastSubscription);
      }
    };

    this.ws.onmessage = function (event) {
      if (typeof event.data === 'string') {
        try {
          self._handleTextMessage(JSON.parse(event.data));
        } catch (e) {
          // ignore bad JSON
        }
      } else {
        self._handleBinaryFrame(event.data);
      }
    };

    this.ws.onclose = function (event) {
      if (gen !== self._connGen) return;
      self.ws = null;
      if (event.code === 4401 || event.code === 4403) {
        var code = event.reason || (event.code === 4403 ? 'insufficient_scope' : 'invalid_key');
        if (code === 'ticket_expired' && !self._intentionalClose) {
          self.emit('status', { connected: false, code: event.code, reason: code, reconnecting: true });
          self._freshTicket = true;
          self._connect();
          return;
        }
        // Auth loss: no automatic reconnect. A new key (trauth:change) reconnects.
        self._authClosed = true;
        self.emit('status', { connected: false, code: event.code, reason: code });
        self.emit('auth_error', { code: code, closeCode: event.code });
        if (window.trAuth && window.trAuth.handleAuthLoss) window.trAuth.handleAuthLoss(code);
        return;
      }
      self.emit('status', { connected: false, code: event.code });
      if (self._intentionalClose || event.code === 1000) return;
      if (!opened) self._freshTicket = true;   // the ticket may have been the problem
      self._scheduleReconnect(opened);
    };

    this.ws.onerror = function () {
      if (gen !== self._connGen) return;
      self.emit('error', { message: 'WebSocket error' });
    };
  }

  // A handshake the server refused (e.g. 401 without a key while anonymous
  // access is off) looks like any network failure to a WebSocket. Before
  // retrying one that never opened, check whether a key is simply required.
  async _scheduleReconnect(opened) {
    var self = this;
    var gen = this._connGen;
    if (!opened && window.trAuth && !window.trAuth.getKey()) {
      var who = null;
      try { who = await window.trAuth.whoami({ refresh: true }); } catch (e) { who = null; }
      if (gen !== this._connGen || this._intentionalClose) return;
      if (who && who.anonymous && who.anonymous.access === 'off') {
        this._authClosed = true;
        this.emit('auth_error', { code: 'key_required', closeCode: 0 });
        if (window.trAuth.handleAuthLoss) window.trAuth.handleAuthLoss('key_required');
        return;
      }
    }
    setTimeout(function () {
      if (gen === self._connGen) self._connect();
    }, this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.options.reconnectMaxMs);
  }

  _handleTextMessage(msg) {
    switch (msg.type) {
      case 'call_start':
        this.emit('call_start', msg);
        break;
      case 'call_end':
        this.emit('call_end', msg);
        break;
      case 'keepalive':
        this.emit('status', { connected: true, active_streams: msg.active_streams });
        break;
      case 'config':
        if (msg.audio_format) {
          this._serverAudioFormat = msg.audio_format;
        }
        this.emit('config', msg);
        break;
    }
  }

  _handleBinaryFrame(buffer) {
    if (buffer.byteLength < 14) return;

    var view = new DataView(buffer);
    var systemId = view.getUint16(0);
    var tgid = view.getUint32(2);
    var serverTs = view.getUint32(6);
    var seq = view.getUint16(10);
    var sampleRate = view.getUint16(12) || 8000;

    var audioData = buffer.slice(14);
    var audioLen = audioData.byteLength;
    var key = systemId + ':' + tgid;

    // --- Jitter tracking ---
    var now = performance.now();
    var entry = this._jitterTracking.get(key);
    if (!entry) {
      entry = this._newJitterEntry();
      this._jitterTracking.set(key, entry);
    }

    // Detect transmission boundary (gap > 500ms)
    if (entry.prevClientTime > 0) {
      var gap = now - entry.prevClientTime;
      if (gap > this._transmissionGapMs && entry.transmission) {
        // End current transmission
        entry.transmission.endTime = entry.prevClientTime;
        entry.transmission.duration = entry.transmission.endTime - entry.transmission.startTime;
        entry.transmission.clientStats = this._snapshotJitterStats(entry.stats);
        entry.transmission.serverStats = this._snapshotJitterStats(entry.serverStats);
        entry.transmission.networkStats = this._snapshotJitterStats(entry.networkStats);
        this._transmissionLog.push(entry.transmission);
        if (this._transmissionLog.length > this._maxTransmissions) {
          this._transmissionLog.shift();
        }
        this.emit('transmission_end', entry.transmission);
        entry.transmission = null;
        this._resetJitterStats(entry.stats);
        this._resetJitterStats(entry.serverStats);
        this._resetJitterStats(entry.networkStats);
        entry.deltas = [];
        entry.deltaIdx = 0;
        entry.prevClientTime = 0;
        entry.prevServerTs = 0;
      }
    }

    // Start new transmission if needed
    if (!entry.transmission) {
      entry.transmission = {
        key: key,
        tgid: tgid,
        systemId: systemId,
        startTime: now,
        endTime: 0,
        duration: 0,
        frameCount: 0,
        seqGaps: 0,
        lastSeq: seq,
        deltas: [],
        audioChunks: [],
        audioSampleRate: 0,
        clientStats: null,
        serverStats: null,
        networkStats: null
      };
      this.emit('transmission_start', entry.transmission);
    }

    entry.transmission.frameCount++;

    // Seq gap detection (skip first frame)
    if (entry.transmission.frameCount > 1) {
      var expectedSeq = (entry.transmission.lastSeq + 1) & 0xFFFF;
      if (seq !== expectedSeq) {
        entry.transmission.seqGaps++;
      }
    }
    entry.transmission.lastSeq = seq;

    // Compute deltas (need at least 2 frames)
    if (entry.prevClientTime > 0 && entry.prevServerTs > 0) {
      var clientDelta = now - entry.prevClientTime;
      var serverDelta = serverTs - entry.prevServerTs;
      var networkJitter = clientDelta - serverDelta;

      var sample = { clientDelta: clientDelta, serverDelta: serverDelta, networkJitter: networkJitter, ts: now };

      // Filter gaps/outliers: skip deltas > 3x the running mean (or > 500ms
      // before the mean stabilizes). These are legitimate pauses in the audio
      // (inter-burst gaps, brief squelch), not jitter — exclude from stats,
      // charts, and transmission records so they don't inflate metrics.
      var isOutlier = (entry.stats.count >= 3 && clientDelta > entry.stats.mean * 3)
        || (entry.stats.count < 3 && clientDelta > 500);

      if (!isOutlier) {
        this._addJitterSample(entry.stats, clientDelta);
        this._addJitterSample(entry.serverStats, serverDelta);
        this._addJitterSample(entry.networkStats, networkJitter);

        entry.transmission.deltas.push(sample);

        if (entry.deltas.length < this._maxDeltas) {
          entry.deltas.push(sample);
        } else {
          entry.deltas[entry.deltaIdx] = sample;
          entry.deltaIdx = (entry.deltaIdx + 1) % this._maxDeltas;
        }
        this.emit('jitter_sample', { key: key, sample: sample });
      }
    }

    entry.prevClientTime = now;
    entry.prevServerTs = serverTs;
    // --- End jitter tracking ---

    if (!this.tgNodes.has(key)) {
      this._createTG(key, tgid, systemId);
    }

    // Determine format: use server-sent config if available, otherwise auto-detect.
    var format = this._serverAudioFormat;
    if (!format) {
      format = (audioLen < 120 && audioLen % 2 !== 0) ? 'opus' : 'pcm';
    }

    if (format === 'pcm' && audioLen >= 2) {
      var pcmData = new Int16Array(audioData);
      this._feedPCM(key, pcmData, sampleRate);
    } else if (format === 'opus' && audioLen > 0) {
      this._decodeOpus(key, new Uint8Array(audioData));
    }
  }

  _feedPCM(key, int16Samples, sampleRate) {
    var nodes = this.tgNodes.get(key);
    if (!nodes) return;

    // Capture audio for debug reports before the buffer is transferred
    var entry = this._jitterTracking.get(key);
    if (entry && entry.transmission) {
      entry.transmission.audioChunks.push(new Int16Array(int16Samples));
      if (!entry.transmission.audioSampleRate) entry.transmission.audioSampleRate = sampleRate;
    }

    var msg = {
      type: 'audio',
      samples: int16Samples,
      sampleRate: sampleRate,
    };
    // Transfer the underlying ArrayBuffer to the worklet thread (zero-copy).
    if (this._useWorklet) {
      nodes.worklet.port.postMessage(msg, [int16Samples.buffer]);
    } else {
      nodes.worklet.port.postMessage(msg);
    }
    nodes.lastActivity = Date.now();
  }

  async _decodeOpus(key, opusData) {
    var nodes = this.tgNodes.get(key);
    if (!nodes) return;

    // Lazy-init Opus decoder for this TG
    if (!nodes.opusDecoder) {
      if (typeof AudioDecoder === 'undefined') {
        console.warn('AudioDecoder not available; Opus frames will be dropped');
        return;
      }

      try {
        var self = this;
        var currentKey = key;
        nodes.opusDecoder = new AudioDecoder({
          output: function(audioData) {
            var float32 = new Float32Array(audioData.numberOfFrames);
            audioData.copyTo(float32, { planeIndex: 0 });
            var int16 = new Int16Array(float32.length);
            for (var i = 0; i < float32.length; i++) {
              int16[i] = Math.max(-32768, Math.min(32767, Math.round(float32[i] * 32768)));
            }
            self._feedPCM(currentKey, int16, audioData.sampleRate);
            audioData.close();
          },
          error: function(e) {
            console.error('Opus decode error:', e);
          }
        });

        nodes.opusDecoder.configure({
          codec: 'opus',
          sampleRate: 8000,
          numberOfChannels: 1,
        });
      } catch (e) {
        console.error('Failed to create Opus decoder:', e);
        return;
      }
    }

    try {
      nodes.opusDecoder.decode(new EncodedAudioChunk({
        type: 'key',
        timestamp: 0,
        data: opusData,
      }));
    } catch (e) {
      // Ignore decode errors for individual frames
    }
  }

  _createTG(key, tgid, systemId) {
    var worklet;
    if (this._useWorklet) {
      worklet = new AudioWorkletNode(this.audioCtx, 'radio-audio-processor', {
        outputChannelCount: [1],
      });
    } else {
      worklet = this._createScriptProcessorShim();
    }

    var compressor = this.audioCtx.createDynamicsCompressor();
    compressor.threshold.value = -20;
    compressor.knee.value = 10;
    compressor.ratio.value = 1; // disabled by default
    compressor.attack.value = 0.003;
    compressor.release.value = 0.15;

    var panner = this.audioCtx.createStereoPanner();

    var gain = this.audioCtx.createGain();
    gain.gain.value = 0; // starts muted; setMute(key, false) enables

    // Load persisted settings
    var savedVol = this._loadSetting('vol_' + key);
    if (savedVol !== null) gain.gain.value = savedVol;

    var savedComp = this._loadSetting('comp_' + key);
    var compEnabled = savedComp === true;
    if (compEnabled) compressor.ratio.value = 3;

    var savedPan = this._loadSetting('pan_' + key);

    // Chain: worklet -> compressor -> panner -> gain -> masterCompressor
    worklet.connect(compressor);
    compressor.connect(panner);
    panner.connect(gain);
    gain.connect(this.masterCompressor);

    var nodeEntry = {
      worklet: worklet,
      compressor: compressor,
      panner: panner,
      gain: gain,
      compressorEnabled: compEnabled,
      muted: true,
      lastActivity: Date.now(),
      tgid: tgid,
      systemId: systemId || 0,
      bufferStats: null, // latest stats from worklet
    };

    // Listen for stats from the worklet
    var self = this;
    worklet.port.onmessage = function(e) {
      if (e.data.type === 'stats') {
        nodeEntry.bufferStats = e.data;
        self.emit('buffer_stats', { key: key, stats: e.data });
      }
    };

    this.tgNodes.set(key, nodeEntry);

    // Pan: deterministic position from tgid (assigned on unmute via setMute).
    // For manual pan, restore saved position now.
    if (!this._autoPan && savedPan !== null) {
      panner.pan.value = savedPan;
    }

    this.emit('tg_created', { key: key, tgid: tgid, systemId: systemId || 0 });
  }

  // ScriptProcessorNode fallback for insecure contexts (no AudioWorklet).
  _createScriptProcessorShim() {
    var ctx = this.audioCtx;
    var bufferSize = 2048;
    var scriptNode = ctx.createScriptProcessor(bufferSize, 0, 1);

    var ringBuf = new Float32Array(32768);
    var writePos = 0;
    var readPos = 0;
    var buffered = 0;
    var inputSampleRate = 8000;
    var resampleAccum = 0;
    var playing = false;
    var silentFrames = 0;

    function enqueueSamples(int16Array, sr) {
      if (sr && sr !== inputSampleRate) {
        inputSampleRate = sr;
      }
      for (var i = 0; i < int16Array.length; i++) {
        ringBuf[writePos] = int16Array[i] / 32768.0;
        writePos = (writePos + 1) % ringBuf.length;
        buffered = Math.min(buffered + 1, ringBuf.length);
      }
      var maxSamples = Math.floor(inputSampleRate * 1.5);
      var targetSamples = Math.floor(inputSampleRate * 0.5);
      if (buffered > maxSamples) {
        var skip = buffered - targetSamples;
        readPos = (readPos + skip) % ringBuf.length;
        buffered -= skip;
      }
    }

    scriptNode.onaudioprocess = function (e) {
      var output = e.outputBuffer.getChannelData(0);
      var outRate = e.outputBuffer.sampleRate;

      if (!playing) {
        var startThreshold = Math.floor(inputSampleRate * 0.2);
        if (buffered >= startThreshold) {
          playing = true;
          silentFrames = 0;
          resampleAccum = 0;
        } else {
          for (var i = 0; i < output.length; i++) output[i] = 0;
          return;
        }
      }

      var ratio = inputSampleRate / outRate;
      var hadData = false;

      for (var i = 0; i < output.length; i++) {
        if (buffered > 0) {
          hadData = true;
          var idx0 = readPos;
          var idx1 = (readPos + 1) % ringBuf.length;
          var frac = resampleAccum;
          output[i] = ringBuf[idx0] * (1 - frac) + (buffered > 1 ? ringBuf[idx1] : ringBuf[idx0]) * frac;
          resampleAccum += ratio;
          while (resampleAccum >= 1.0 && buffered > 0) {
            resampleAccum -= 1.0;
            readPos = (readPos + 1) % ringBuf.length;
            buffered--;
          }
        } else {
          output[i] = 0;
        }
      }

      if (!hadData) {
        silentFrames++;
        if (silentFrames > 188) {
          playing = false;
          silentFrames = 0;
        }
      } else {
        silentFrames = 0;
      }
    };

    var active = true;
    scriptNode.port = {
      postMessage: function (msg) {
        if (msg.type === 'audio') {
          enqueueSamples(msg.samples, msg.sampleRate);
        } else if (msg.type === 'stop') {
          active = false;
        }
      }
    };

    return scriptNode;
  }

  _removeTG(key) {
    var nodes = this.tgNodes.get(key);
    if (!nodes) return;
    nodes.worklet.port.postMessage({ type: 'stop' });
    nodes.worklet.disconnect();
    nodes.compressor.disconnect();
    if (nodes.panner) nodes.panner.disconnect();
    nodes.gain.disconnect();
    if (nodes.opusDecoder) {
      try { nodes.opusDecoder.close(); } catch (e) { /* ignore */ }
    }
    this.tgNodes.delete(key);
    this.emit('tg_removed', { key: key, tgid: nodes.tgid, systemId: nodes.systemId });
  }

  _saveSetting(key, value) {
    try {
      var settings = JSON.parse(localStorage.getItem('audio-engine') || '{}');
      settings[key] = value;
      localStorage.setItem('audio-engine', JSON.stringify(settings));
    } catch (e) {
      // ignore storage errors
    }
  }

  _loadSetting(key) {
    try {
      var settings = JSON.parse(localStorage.getItem('audio-engine') || '{}');
      return settings[key] ?? null;
    } catch (e) {
      return null;
    }
  }

  _loadSettings() {
    var masterVol = this._loadSetting('master_vol');
    if (masterVol !== null && this.masterGain) this.masterGain.gain.value = masterVol;

    var autoPan = this._loadSetting('auto_pan');
    if (autoPan !== null) this._autoPan = autoPan;

    var masterComp = this._loadSetting('master_comp');
    if (masterComp === false && this.masterCompressor) this.masterCompressor.ratio.value = 1;
  }
}

// Export for use by pages
window.AudioEngine = AudioEngine;
