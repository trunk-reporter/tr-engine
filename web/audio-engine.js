// Main thread audio coordinator for live radio streaming.
// Manages WebSocket connection, per-TG audio nodes, mixing, and compression.
// Usage: const engine = new AudioEngine(); await engine.start(); engine.subscribe({tgids: [1234]});

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
    this.tgNodes = new Map(); // tgid -> { worklet, gain, compressor, panner, compressorEnabled, muted, lastActivity }
    this.reconnectDelay = 1000;
    this.lastSubscription = null;
    this.listeners = {};
    this._intentionalClose = false;
    this._serverAudioFormat = null; // set by server 'config' message; null = auto-detect
    this._autoPan = true; // auto-distribute channels across stereo field
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
    if (this.audioCtx.state === 'suspended') {
      await this.audioCtx.resume();
    }

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
    this._connect();
  }

  stop() {
    this._intentionalClose = true;
    if (this.ws) {
      this.ws.close(1000);
      this.ws = null;
    }
    var self = this;
    this.tgNodes.forEach(function (nodes, tgid) {
      self._removeTG(tgid);
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

  setVolume(tgid, value) {
    var nodes = this.tgNodes.get(tgid);
    if (nodes) nodes.gain.gain.value = value;
    this._saveSetting('vol_' + tgid, value);
  }

  getVolume(tgid) {
    var nodes = this.tgNodes.get(tgid);
    return nodes ? nodes.gain.gain.value : 1.0;
  }

  setMute(tgid, muted) {
    var nodes = this.tgNodes.get(tgid);
    if (nodes) {
      nodes.muted = muted;
      nodes.gain.gain.value = muted ? 0 : (this._loadSetting('vol_' + tgid) ?? 1.0);
      if (!muted && this._autoPan && !nodes._panAssigned) {
        nodes.panner.pan.value = this._panForTgid(tgid);
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

  setTGCompressorEnabled(tgid, enabled) {
    var nodes = this.tgNodes.get(tgid);
    if (!nodes) return;
    nodes.compressorEnabled = enabled;
    nodes.compressor.ratio.value = enabled ? 3 : 1;
    this._saveSetting('comp_' + tgid, enabled);
  }

  setPan(tgid, value) {
    var nodes = this.tgNodes.get(tgid);
    if (nodes && nodes.panner) nodes.panner.pan.value = Math.max(-1, Math.min(1, value));
    this._saveSetting('pan_' + tgid, value);
  }

  getPan(tgid) {
    var nodes = this.tgNodes.get(tgid);
    return nodes && nodes.panner ? nodes.panner.pan.value : 0;
  }

  setAutoPan(enabled) {
    this._autoPan = enabled;
    this._saveSetting('auto_pan', enabled);
    if (enabled) {
      // Assign pan to all unmuted TGs that don't have one yet
      var self = this;
      this.tgNodes.forEach(function(nodes, tgid) {
        if (!nodes.muted && !nodes._panAssigned) {
          nodes.panner.pan.value = self._panForTgid(tgid);
          nodes._panAssigned = true;
        }
      });
    }
  }

  getAutoPan() {
    return this._autoPan;
  }

  // Deterministic pan position from tgid — always the same, never moves
  _panForTgid(tgid) {
    // Hash tgid to spread across -0.8..0.8
    var h = ((tgid * 2654435761) >>> 0) % 10000;
    return -0.8 + (h / 10000) * 1.6;
  }

  getActiveTGs() {
    var result = [];
    this.tgNodes.forEach(function (nodes, tgid) {
      result.push({
        tgid: tgid,
        systemId: nodes.systemId || 0,
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

  // --- Internal ---

  _connect() {
    var self = this;
    var protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var token = (window.trAuth && window.trAuth.getToken()) || window._authToken || '';
    var url = protocol + '//' + location.host + this.wsPath + '?token=' + encodeURIComponent(token);

    this.ws = new WebSocket(url);
    this.ws.binaryType = 'arraybuffer';

    this.ws.onopen = function () {
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
      self.emit('status', { connected: false, code: event.code });
      if (!self._intentionalClose && event.code !== 1000) {
        setTimeout(function () { self._connect(); }, self.reconnectDelay);
        self.reconnectDelay = Math.min(self.reconnectDelay * 2, self.options.reconnectMaxMs);
      }
    };

    this.ws.onerror = function () {
      self.emit('error', { message: 'WebSocket error' });
    };
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
    if (buffer.byteLength < 12) return;

    var view = new DataView(buffer);
    var systemId = view.getUint16(0);
    var tgid = view.getUint32(2);
    // timestamp at offset 6 (4 bytes) - available for latency measurement
    // seq at offset 10 (2 bytes) - available for gap detection

    var audioData = buffer.slice(12);
    var audioLen = audioData.byteLength;

    if (!this.tgNodes.has(tgid)) {
      this._createTG(tgid, systemId);
    } else {
      // Update systemId on existing entry (defensive — shouldn't change)
      var existing = this.tgNodes.get(tgid);
      if (existing) existing.systemId = systemId;
    }

    // Determine format: use server-sent config if available, otherwise auto-detect.
    // PCM frames must be even-length (int16 pairs) and at least 20 bytes.
    // Opus frames are typically 10-80 bytes after compression.
    // Default to PCM for ambiguous small frames to avoid mis-detecting
    // trailing PCM fragments as Opus (which could corrupt the sample rate).
    var format = this._serverAudioFormat;
    if (!format) {
      // Only treat as Opus if very small AND odd-length (PCM is always even)
      format = (audioLen < 120 && audioLen % 2 !== 0) ? 'opus' : 'pcm';
    }

    if (format === 'pcm' && audioLen >= 2) {
      var pcmData = new Int16Array(audioData);
      this._feedPCM(tgid, pcmData, 8000);
    } else if (format === 'opus' && audioLen > 0) {
      this._decodeOpus(tgid, new Uint8Array(audioData));
    }
  }

  _feedPCM(tgid, int16Samples, sampleRate) {
    var nodes = this.tgNodes.get(tgid);
    if (!nodes) return;
    nodes.worklet.port.postMessage({
      type: 'audio',
      samples: int16Samples,
      sampleRate: sampleRate,
    });
    nodes.lastActivity = Date.now();
  }

  async _decodeOpus(tgid, opusData) {
    var nodes = this.tgNodes.get(tgid);
    if (!nodes) return;

    // Lazy-init Opus decoder for this TG
    if (!nodes.opusDecoder) {
      if (typeof AudioDecoder === 'undefined') {
        // Browser doesn't support WebCodecs (e.g. Firefox) — drop Opus frames
        console.warn('AudioDecoder not available; Opus frames will be dropped');
        return;
      }

      try {
        var self = this;
        var currentTgid = tgid;
        nodes.opusDecoder = new AudioDecoder({
          output: function(audioData) {
            // Convert decoded AudioData to Float32Array, then to Int16
            var float32 = new Float32Array(audioData.numberOfFrames);
            audioData.copyTo(float32, { planeIndex: 0 });
            var int16 = new Int16Array(float32.length);
            for (var i = 0; i < float32.length; i++) {
              int16[i] = Math.max(-32768, Math.min(32767, Math.round(float32[i] * 32768)));
            }
            self._feedPCM(currentTgid, int16, audioData.sampleRate);
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

  _createTG(tgid, systemId) {
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
    gain.gain.value = 0; // starts muted; setMute(tgid, false) enables

    // Load persisted settings
    var savedVol = this._loadSetting('vol_' + tgid);
    if (savedVol !== null) gain.gain.value = savedVol;

    var savedComp = this._loadSetting('comp_' + tgid);
    var compEnabled = savedComp === true;
    if (compEnabled) compressor.ratio.value = 3;

    var savedPan = this._loadSetting('pan_' + tgid);

    // Chain: worklet -> compressor -> panner -> gain -> masterCompressor
    worklet.connect(compressor);
    compressor.connect(panner);
    panner.connect(gain);
    gain.connect(this.masterCompressor);

    this.tgNodes.set(tgid, {
      worklet: worklet,
      compressor: compressor,
      panner: panner,
      gain: gain,
      compressorEnabled: compEnabled,
      muted: true,
      lastActivity: Date.now(),
      systemId: systemId || 0,
    });

    // Pan: deterministic position from tgid (assigned on unmute via setMute).
    // For manual pan, restore saved position now.
    if (!this._autoPan && savedPan !== null) {
      panner.pan.value = savedPan;
    }

    this.emit('tg_created', { tgid: tgid, systemId: systemId || 0 });
  }

  // ScriptProcessorNode fallback for insecure contexts (no AudioWorklet).
  // Returns an object with the same interface as AudioWorkletNode: .port.postMessage(), .connect(), .disconnect()
  _createScriptProcessorShim() {
    var ctx = this.audioCtx;
    var bufferSize = 2048;
    var scriptNode = ctx.createScriptProcessor(bufferSize, 0, 1);

    // Ring buffer (mirrors audio-worklet.js logic)
    var ringBuf = new Float32Array(16384);
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
        var startThreshold = Math.floor(inputSampleRate * 0.3);
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
        if (silentFrames > 37) {
          playing = false;
          silentFrames = 0;
        }
      } else {
        silentFrames = 0;
      }
    };

    // Shim the AudioWorkletNode interface expected by the rest of the engine
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

  _removeTG(tgid) {
    var nodes = this.tgNodes.get(tgid);
    if (!nodes) return;
    nodes.worklet.port.postMessage({ type: 'stop' });
    nodes.worklet.disconnect();
    nodes.compressor.disconnect();
    if (nodes.panner) nodes.panner.disconnect();
    nodes.gain.disconnect();
    if (nodes.opusDecoder) {
      try { nodes.opusDecoder.close(); } catch (e) { /* ignore */ }
    }
    this.tgNodes.delete(tgid);
    this.emit('tg_removed', { tgid: tgid });
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
