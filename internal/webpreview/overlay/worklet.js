// clank-pcm: AudioWorklet processor for push-to-talk capture.
//
// The overlay creates its AudioContext at 16 kHz (matching the engine's
// expected rate — the browser resamples the mic internally), so this
// processor only converts float32 → s16le and batches ~128 ms per
// message to keep the WebSocket frame rate sane. It also reports an
// RMS level per batch for the mic-button meter.
class ClankPCM extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buf = new Int16Array(2048); // 128 ms at 16 kHz
    this.n = 0;
    this.sq = 0;
  }
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (!ch) return true;
    for (let i = 0; i < ch.length; i++) {
      const s = Math.max(-1, Math.min(1, ch[i]));
      this.sq += s * s;
      this.buf[this.n++] = (s * 0x7fff) | 0;
      if (this.n === this.buf.length) this.flush();
    }
    return true;
  }
  flush() {
    if (this.n === 0) return;
    const out = this.buf.slice(0, this.n);
    const level = Math.sqrt(this.sq / this.n);
    this.port.postMessage({ pcm: out, level }, [out.buffer]);
    this.n = 0;
    this.sq = 0;
  }
}
registerProcessor('clank-pcm', ClankPCM);
