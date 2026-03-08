// AudioWorklet processor with jitter buffer for live radio audio.
// Receives PCM int16 samples via port.postMessage, outputs at AudioContext sample rate.

class RadioAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(16384); // ~2s ring buffer at 8kHz
    this.writePos = 0;
    this.readPos = 0;
    this.buffered = 0;
    this.inputSampleRate = 8000;
    this.active = true;
    this.resampleAccum = 0; // fractional sample accumulator for resampling
    this.playing = false; // playout state: false = buffering, true = playing
    this.silentFrames = 0; // consecutive process() calls with no data

    this.port.onmessage = (e) => {
      if (e.data.type === 'audio') {
        this.enqueueSamples(e.data.samples, e.data.sampleRate);
      } else if (e.data.type === 'stop') {
        this.active = false;
      }
    };
  }

  enqueueSamples(int16Array, sr) {
    if (sr && sr !== this.inputSampleRate) {
      this.inputSampleRate = sr;
    }

    for (let i = 0; i < int16Array.length; i++) {
      this.buffer[this.writePos] = int16Array[i] / 32768.0;
      this.writePos = (this.writePos + 1) % this.buffer.length;
      this.buffered = Math.min(this.buffered + 1, this.buffer.length);
    }

    // Overflow protection: only trim when buffer exceeds 1.5s (excessive latency)
    // Normal operation: buffer hovers at 0-400ms during active calls
    const maxSamples = Math.floor(this.inputSampleRate * 1.5);
    const targetSamples = Math.floor(this.inputSampleRate * 0.5);
    if (this.buffered > maxSamples) {
      const skip = this.buffered - targetSamples;
      this.readPos = (this.readPos + skip) % this.buffer.length;
      this.buffered -= skip;
    }
  }

  process(inputs, outputs, parameters) {
    if (!this.active) return false;

    const output = outputs[0][0]; // mono
    if (!output) return true;

    // Jitter buffer with soft underrun handling:
    // - Accumulate 300ms before first playout (absorb initial jitter)
    // - During playback, ride through brief underruns with silence
    // - Only fully stop after 100ms of sustained empty buffer (~38 process() calls)
    if (!this.playing) {
      const startThreshold = Math.floor(this.inputSampleRate * 0.3); // 300ms
      if (this.buffered >= startThreshold) {
        this.playing = true;
        this.silentFrames = 0;
        this.resampleAccum = 0; // fresh start
      } else {
        // Still buffering — output silence
        for (let i = 0; i < output.length; i++) {
          output[i] = 0;
        }
        return true;
      }
    }

    // ratio < 1 when upsampling (e.g. 8000/48000 = 0.167)
    // ratio > 1 when downsampling
    const ratio = this.inputSampleRate / sampleRate;
    let hadData = false;

    for (let i = 0; i < output.length; i++) {
      if (this.buffered > 0) {
        hadData = true;
        // Linear interpolation between input samples
        const idx0 = this.readPos;
        const idx1 = (this.readPos + 1) % this.buffer.length;
        const frac = this.resampleAccum;
        output[i] = this.buffer[idx0] * (1 - frac) + (this.buffered > 1 ? this.buffer[idx1] : this.buffer[idx0]) * frac;

        // Advance fractional position by ratio
        this.resampleAccum += ratio;

        // Consume whole input samples
        while (this.resampleAccum >= 1.0 && this.buffered > 0) {
          this.resampleAccum -= 1.0;
          this.readPos = (this.readPos + 1) % this.buffer.length;
          this.buffered--;
        }
      } else {
        output[i] = 0; // brief underrun — output silence but keep playing
      }
    }

    // Track sustained underruns: only stop after ~100ms of empty buffer
    if (!hadData) {
      this.silentFrames++;
      // 100ms at 48kHz with 128-sample blocks = ~37 frames
      if (this.silentFrames > 37) {
        this.playing = false;
        this.silentFrames = 0;
      }
    } else {
      this.silentFrames = 0;
    }

    return true;
  }
}

registerProcessor('radio-audio-processor', RadioAudioProcessor);
