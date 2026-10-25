/* Capture mono speech, low-pass before conversion to the modem's 4 kHz stream.
 * No microphone samples are sent to the local speakers or saved. */
class CallMicrophoneProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.silence = new Float32Array(128);
    this.history = new Float32Array(63); this.position = 0; this.phase = 0;
    this.frame = new Int16Array(80); this.offset = 0;
    this.coefficients = new Float32Array(63);
    const cutoff = 1700 / sampleRate;
    let total = 0;
    for (let i = 0; i < 63; i++) {
      const x = i - 31;
      const sinc = x === 0 ? 2 * cutoff : Math.sin(2 * Math.PI * cutoff * x) / (Math.PI * x);
      const value = sinc * (.54 - .46 * Math.cos(2 * Math.PI * i / 62));
      this.coefficients[i] = value; total += value;
    }
    for (let i = 0; i < 63; i++) this.coefficients[i] /= total;
  }
  process(inputs) {
    // Disabled or temporarily muted tracks may provide no input channels.
    // Continue the clock with silence so muting cannot expire the USB session.
    const input = inputs[0]?.[0] || this.silence;
    for (const sample of input) {
      this.history[this.position] = sample;
      this.position = (this.position + 1) % 63;
      this.phase += 4000;
      if (this.phase < sampleRate) continue;
      this.phase -= sampleRate;
      let value = 0;
      for (let i = 0; i < 63; i++) value += this.history[(this.position + i) % 63] * this.coefficients[i];
      this.frame[this.offset++] = Math.round(Math.max(-1, Math.min(1, value)) * 32767);
      if (this.offset === 80) {
        // Explicit little endian; typed-array byte order is host-dependent.
        const bytes = new ArrayBuffer(160), view = new DataView(bytes);
        for (let i = 0; i < 80; i++) view.setInt16(i * 2, this.frame[i], true);
        this.port.postMessage(bytes, [bytes]); this.offset = 0;
      }
    }
    return true;
  }
}
registerProcessor("call-microphone", CallMicrophoneProcessor);
