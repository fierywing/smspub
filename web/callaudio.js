/* USB 通话声音与麦克风；仅本站传输，不保存音频。 */
"use strict";
window.CallAudio = class {
  constructor(onChange) {
    this.onChange = onChange;
    this.sources = new Set();
    this.volume = .8; this.micGeneration = 0;
  }
  get running() { return !!this.session; }
  emit(message, state = "connected") { this.onChange?.({message, state, cardId: this.cardId}); }
  stop(message = "收听已关闭") {
    ++this.micGeneration;
    this.preparedMedia?.getTracks().forEach(track => track.stop()); this.preparedMedia = null; this.preparedCard = null;
    this.media?.getTracks().forEach(track => track.stop()); this.media = null;
    this.connection?.controller.abort(); this.connection = null;
    this.disconnectCapture();
    this.microphoneEnabled = false; this.microphonePending = false; this.microphoneCard = null; this.muted = false;
    this.session?.abort();
    this.session = null;
    for (const source of this.sources) { try { source.stop(); source.disconnect(); } catch {} }
    this.sources.clear();
    this.audio?.close().catch(() => {});
    this.audio = null; this.workletAudio = null; this.gain = null; this.cardId = null;
    this.emit(message, "stopped");
  }
  setVolume(volume) {
    this.volume = Math.max(0, Math.min(1, Number(volume)));
    if (this.gain) this.gain.gain.value = this.volume;
  }
  async armMicrophone(cardId) {
    if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia) {
      this.emit("麦克风需要通过 HTTPS 或 localhost 打开网页", "error"); return;
    }
    if (this.preparedMedia || this.microphonePending) return;
    const generation = ++this.micGeneration;
    this.microphoneCard = cardId;
    this.microphonePending = true; this.emit("请允许浏览器使用麦克风…", "connecting");
    try {
      const media = await navigator.mediaDevices.getUserMedia({audio: {channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true}});
      if (generation !== this.micGeneration) { media.getTracks().forEach(t => t.stop()); return; }
      this.preparedMedia = media; this.preparedCard = cardId; this.microphonePending = false;
      this.emit("麦克风已就绪，等待电话接通…", "prepared");
    } catch (error) {
      if (generation !== this.micGeneration) return;
      this.microphonePending = false; this.microphoneCard = null;
      this.emit(error.name === "NotAllowedError" ? "麦克风权限未允许，仍可收听对方" : "无法访问麦克风，请检查设备", "error");
    }
  }
  cancelPreparedMicrophone() {
    ++this.micGeneration;
    this.preparedMedia?.getTracks().forEach(track => track.stop());
    this.preparedMedia = null; this.preparedCard = null;
    this.microphonePending = false; this.microphoneCard = null;
    this.emit("麦克风已关闭", this.running ? "connected" : "stopped");
  }
  async enableMicrophone(cardId) {
    if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia)
      return this.emit("麦克风需要通过 HTTPS 或 localhost 打开网页", "error");
    if (this.microphonePending) return;
    const generation = ++this.micGeneration;
    this.microphoneCard = cardId;
    this.microphonePending = true; this.emit("请允许浏览器使用麦克风…", "connecting");
    try {
      const media = await navigator.mediaDevices.getUserMedia({audio: {channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true}});
      if (generation !== this.micGeneration) { media.getTracks().forEach(t => t.stop()); return; }
      this.start(cardId, "stereo", {media});
    } catch (error) {
      if (generation !== this.micGeneration) return;
      this.microphonePending = false; this.microphoneCard = null;
      this.emit(error.name === "NotAllowedError" ? "麦克风权限未允许，可在浏览器设置中开启" : "无法访问麦克风，请检查设备", "error");
    }
  }
  toggleMute() {
    this.muted = !this.muted;
    this.media?.getAudioTracks().forEach(track => { track.enabled = !this.muted; });
    this.emit(this.muted ? "麦克风已静音，仍可收听对方" : "麦克风已开启，正在通话", "playing");
  }
  disconnectCapture() {
    if (this.micNode) { this.micNode.port.onmessage = null; this.micNode.onprocessorerror = null; this.micNode.disconnect(); }
    this.micNode = null;
    this.micSource?.disconnect(); this.micSource = null;
    this.micSink?.disconnect(); this.micSink = null;
  }
  pause(ms, signal) {
    return new Promise(resolve => {
      const done = () => { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); };
      const timer = setTimeout(done, ms);
      signal.addEventListener("abort", done, {once: true});
      if (signal.aborted) done();
    });
  }
  async start(cardId, channel = "stereo", {media} = {}) {
    this.stop();
    const session = new AbortController();
    this.session = session; this.cardId = cardId; this.media = media; this.microphonePending = !!media; this.microphoneCard = media ? cardId : null;
    this.emit("正在连接通话声音…", "connecting");
    try {
      const Audio = window.AudioContext || window.webkitAudioContext;
      if (!Audio) throw new Error("当前浏览器不支持音频播放");
      const audio = new Audio({latencyHint: "interactive"});
      this.audio = audio;
      this.gain = audio.createGain(); this.gain.gain.value = this.volume;
      this.gain.connect(audio.destination);
      await audio.resume();
      media?.getAudioTracks().forEach(track => track.addEventListener("ended", () => {
        if (this.session === session) this.stop("麦克风设备已断开");
      }, {once: true, signal: session.signal}));
      let failures = 0;
      while (this.session === session && !session.signal.aborted) {
        const since = performance.now();
        try {
          await this.connectAudio(cardId, channel, media, session, audio);
        } catch (error) {
          if (session.signal.aborted || this.session !== session) return;
          if (error.retry === false) throw error;
          if (performance.now() - since > 10000) failures = 0;
          failures = Math.min(failures + 1, 5);
          this.microphonePending = false;
          this.emit("通话声音暂时中断，正在自动重连…", "reconnecting");
          await this.pause(Math.min(5000, 500 * 2 ** (failures - 1)), session.signal);
        }
      }
    } catch (error) {
      if (this.session === session && !session.signal.aborted) {
        this.stop(error.message); this.emit(error.message, "error");
      }
    }
  }
  async connectAudio(cardId, channel, media, session, audio) {
    const controller = new AbortController(), connection = {controller};
    this.connection = connection;
    const abort = () => controller.abort();
    session.signal.addEventListener("abort", abort, {once: true});
    let reader, watchdog, lastReceived = performance.now();
    try {
      // Status events arrive every 500 ms, even during silence.
      watchdog = setInterval(() => {
        if (performance.now() - lastReceived > 10000) {
          connection.failure = new Error("音频连接长时间无响应"); controller.abort();
        }
      }, 1000);
      const response = await fetch(`/api/cards/${encodeURIComponent(cardId)}/audio?channel=${encodeURIComponent(channel)}${media ? "&microphone=1" : ""}`, {
        headers: {"X-Call-Audio": "1"}, signal: controller.signal, cache: "no-store"
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw Object.assign(new Error(body.error || "无法连接通话声音"), {
          retry: ![403, 404].includes(response.status) && !body.error?.includes("接通后才能")
        });
      }
      reader = response.body.getReader();
      const decoder = new TextDecoder();
      let pending = "", format, nextAt = 0;
      const receivedAt = performance.now();
      while (!controller.signal.aborted) {
        const {done, value} = await reader.read();
        if (done) throw new Error("音频连接中断");
        lastReceived = performance.now();
        if (this.session !== session) return;
        pending += decoder.decode(value, {stream: true});
        if (pending.length > 200000) throw new Error("音频连接数据异常");
        let newline;
        while ((newline = pending.indexOf("\n")) >= 0) {
          const event = JSON.parse(pending.slice(0, newline));
          pending = pending.slice(newline + 1);
          if (event.type === "error") throw Object.assign(new Error(event.message), {retry: event.retry !== false});
          if (event.type === "format") {
            if (event.rate !== 16000 || ![1, 2].includes(event.channels)) throw new Error("音频格式不支持");
            format = event;
            if (media) {
              if (!event.microphone || event.uplinkRate !== 4000 || event.uplinkFrameBytes !== 160 || !event.session)
                throw new Error("模组未接受麦克风连接");
              await this.captureMicrophone(audio, media, event.session, cardId, session, connection);
              if (session.signal.aborted) return;
              this.microphoneEnabled = true; this.microphonePending = false;
              this.emit(this.muted ? "麦克风已静音，仍可收听对方" : "麦克风已开启，正在通话", "playing");
            }
          } else if (event.type === "status") {
            this.emit(this.microphoneEnabled ? (this.muted ? "麦克风已静音，仍可收听对方" : "麦克风已开启，正在通话") :
              this.preparedMedia ? "麦克风已就绪，等待电话接通…" : event.frames ? "正在收听通话声音" :
              !event.bytes && performance.now() - receivedAt > 3000 ? "未收到音频日志，请检查模组日志开关" : "声音已连接，等待电话接通…",
              event.frames ? "playing" : "waiting");
          } else if (event.type === "pcm" && format) {
            const now = audio.currentTime;
            // 后台标签/网络积压时跳帧，不让队列和延迟无限增长。
            if (nextAt - now > .3) continue;
            nextAt = Math.max(nextAt, now + .05);
            const bytes = Uint8Array.from(atob(event.data), c => c.charCodeAt(0));
            if (bytes.length !== 640 * format.channels) throw new Error("音频帧长度异常");
            const view = new DataView(bytes.buffer);
            const buffer = audio.createBuffer(format.channels, 320, format.rate);
            for (let c = 0; c < format.channels; c++) {
              const samples = buffer.getChannelData(c);
              for (let i = 0; i < 320; i++) samples[i] = view.getInt16((i * format.channels + c) * 2, true) / 32768;
            }
            const source = audio.createBufferSource();
            source.buffer = buffer; source.connect(this.gain);
            this.sources.add(source);
            source.onended = () => { this.sources.delete(source); source.disconnect(); };
            source.start(nextAt); nextAt += .02;
          }
        }
      }
      if (connection.failure) throw connection.failure;
    } catch (error) {
      throw connection.failure || error;
    } finally {
      clearInterval(watchdog);
      controller.abort();
      await reader?.cancel().catch(() => {});
      session.signal.removeEventListener("abort", abort);
      if (this.connection === connection) {
        this.connection = null; this.disconnectCapture();
        // Retain the original microphone and mute choice across a reconnect.
        for (const source of this.sources) { try { source.stop(); source.disconnect(); } catch {} }
        this.sources.clear();
      }
    }
  }
  async captureMicrophone(audio, media, token, cardId, session, connection) {
    if (!audio.audioWorklet) throw new Error("当前浏览器不支持实时麦克风处理");
    if (!this.workletAudio || this.workletAudio !== audio) {
      await audio.audioWorklet.addModule("/microphone-worklet.js"); this.workletAudio = audio;
    }
    if (session.signal.aborted || connection.controller.signal.aborted) return;
    const source = audio.createMediaStreamSource(media);
    const node = new AudioWorkletNode(audio, "call-microphone", {numberOfInputs: 1, numberOfOutputs: 1, channelCount: 1});
    const sink = audio.createGain(); sink.gain.value = 0;
    source.connect(node); node.connect(sink); sink.connect(audio.destination);
    this.micSource = source; this.micNode = node; this.micSink = sink;
    let frames = [], posting = false, sequence = 0, lastFrameAt = performance.now(), failures = 0;
    const fail = error => {
      if (this.session !== session || connection.controller.signal.aborted) return;
      connection.failure = error; connection.controller.abort();
    };
    const send = async () => {
      if (posting || connection.controller.signal.aborted || frames.length < 5) return;
      posting = true;
      const chunk = frames.splice(0, 5), bytes = new Uint8Array(800);
      chunk.forEach((frame, i) => bytes.set(frame, i * 160));
      const upload = new AbortController();
      const abort = () => upload.abort();
      connection.controller.signal.addEventListener("abort", abort, {once: true});
      const timeout = setTimeout(abort, 5000);
      try {
        const data = btoa(String.fromCharCode(...bytes));
        const response = await fetch(`/api/cards/${encodeURIComponent(cardId)}/audio/microphone`, {
          method: "POST", headers: {"Content-Type": "application/json", "X-Call-Audio": "1"},
          body: JSON.stringify({session: token, sequence: sequence++, data}), signal: upload.signal
        });
        if (!response.ok) { const body = await response.json().catch(() => ({})); throw Object.assign(new Error(body.error || "麦克风发送失败"), {reconnect: response.status === 409}); }
        failures = 0;
      } catch (error) {
        // Drop the failed chunk: retrying old speech would increase call delay.
        if (error.reconnect || ++failures >= 3) fail(error);
      } finally {
        clearTimeout(timeout); connection.controller.signal.removeEventListener("abort", abort); posting = false;
      }
      if (!connection.controller.signal.aborted && frames.length >= 5) send();
    };
    node.port.onmessage = ({data}) => {
      if (connection.controller.signal.aborted || this.session !== session) return;
      // A backgrounded tab must discard stale speech rather than replay it.
      const now = performance.now(); if (now - lastFrameAt > 300) frames = [];
      lastFrameAt = now;
      frames.push(this.muted ? new Uint8Array(160) : new Uint8Array(data));
      if (frames.length > 10) frames.splice(0, frames.length - 10);
      send();
    };
    // Keep sending inaudible zero PCM whenever capture supplies no frames,
    // including a suspended browser audio graph. Call end or explicit stop
    // cancels this timer; a quiet microphone never ends the audio session.
    const silenceTimer = setInterval(() => {
      if (connection.controller.signal.aborted || this.session !== session) return;
      if (performance.now() - lastFrameAt > 200) {
        frames = Array.from({length: 5}, () => new Uint8Array(160)); send();
      }
    }, 100);
    connection.controller.signal.addEventListener("abort", () => clearInterval(silenceTimer), {once: true});
    node.onprocessorerror = () => fail(new Error("麦克风处理已停止，正在重新连接"));
  }
};
