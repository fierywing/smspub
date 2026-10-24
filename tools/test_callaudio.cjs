// Run with: node --test tools/test_callaudio.cjs. No modem or telephone calls.
const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '../web/callaudio.js'), 'utf8');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(condition) {
  const end = Date.now() + 2000;
  while (!condition()) { if (Date.now() > end) throw new Error('condition timed out'); await delay(5); }
}
function fixture() {
  const streams = [], posts = [], states = [];
  let upload, rendering = true;
  class Track extends EventTarget {
    readyState = 'live'; enabled = true;
    stop() { this.readyState = 'ended'; }
  }
  const track = new Track(), media = {getTracks: () => [track], getAudioTracks: () => [track]};
  class Audio {
    currentTime = 0; destination = {}; audioWorklet = {addModule: async () => {}};
    createGain() { return {gain: {}, connect() {}, disconnect() {}}; }
    createMediaStreamSource() { return {connect() {}, disconnect() {}}; }
    async resume() {} async close() {}
  }
  class Worklet {
    port = {};
    connect() { this.timer = setInterval(() => rendering && this.port.onmessage?.({data: new Uint8Array(160).fill(18).buffer}), 2); }
    disconnect() { clearInterval(this.timer); }
  }
  const fetch = async (url, options) => {
    if (url.endsWith('/microphone')) {
      const body = JSON.parse(options.body); posts.push(body);
      return upload ? upload(body, options) : new Response('{}');
    }
    let controller;
    const response = new Response(new ReadableStream({
      start(c) {
        controller = c;
        c.enqueue(new TextEncoder().encode(JSON.stringify({type: 'format', rate: 16000, channels: 2,
          microphone: true, session: `session${streams.length}`, uplinkRate: 4000, uplinkFrameBytes: 160}) + '\n'));
        options.signal.addEventListener('abort', () => { try { c.error(new DOMException('aborted','AbortError')); } catch {} }, {once:true});
      }
    }));
    streams.push({controller, signal: options.signal}); return response;
  };
  const globals = {window: {AudioContext: Audio}, navigator: {}, AudioWorkletNode: Worklet,
    AbortController, TextDecoder, Uint8Array, DataView, performance, setTimeout, clearTimeout,
    setInterval, clearInterval, fetch, atob, btoa};
  vm.runInNewContext(source, globals);
  const audio = new globals.window.CallAudio(event => states.push(event));
  audio.pause = async (_, signal) => { if (!signal.aborted) await delay(5); };
  return {audio, media, track, streams, posts, states, setUpload: fn => { upload = fn; }, setRendering: value => { rendering = value; }};
}
test('USB error reconnects both directions with the same microphone and mute setting', async t => {
  const f = fixture(); t.after(() => f.audio.stop());
  f.audio.start('card3', 'stereo', {media:f.media}); await until(() => f.posts.length > 0);
  f.audio.toggleMute();
  f.streams[0].controller.enqueue(new TextEncoder().encode('{"type":"error","message":"USB interrupted","retry":true}\n'));
  await until(() => f.streams.length === 2 && f.posts.some(p => p.session === 'session1'));
  assert.equal(f.track.readyState, 'live'); assert.equal(f.audio.media, f.media); assert.equal(f.audio.muted, true);
  assert.equal(f.streams[0].signal.aborted, true);
  const latest = f.posts.filter(p => p.session === 'session1');
  assert.equal(Buffer.from(latest.at(-1).data,'base64').some(v => v !== 0), false);
  assert.ok(f.states.some(s => s.state === 'reconnecting'));
});
test('receive-only EOF automatically reconnects', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3');
  await until(() => f.streams.length === 1); f.streams[0].controller.close();
  await until(() => f.streams.length === 2); assert.equal(f.audio.running,true);
});
test('one failed upload drops old speech and keeps reception open', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); let attempts = 0;
  f.setUpload(async () => { if (++attempts === 1) throw new TypeError('temporary network error'); return new Response('{}'); });
  f.audio.start('card3','stereo',{media:f.media}); await until(() => attempts > 3);
  assert.equal(f.streams.length,1); assert.equal(f.track.readyState,'live');
  assert.deepEqual(f.posts.map(p=>p.sequence), f.posts.map((_,i)=>i));
});
test('repeated upload failures reconnect and replace the expired session', async t => {
  const f = fixture(); t.after(() => f.audio.stop());
  f.setUpload(async body => { if(body.session === 'session0') throw new TypeError('network error'); return new Response('{}'); });
  f.audio.start('card3','stereo',{media:f.media}); await until(() => f.posts.some(p => p.session === 'session1'));
  assert.equal(f.track.readyState,'live'); assert.equal(f.audio.microphoneEnabled,true);
});
test('a confirmed call-end stops capture without reconnecting', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3','stereo',{media:f.media});
  await until(() => f.posts.length > 0);
  f.streams[0].controller.enqueue(new TextEncoder().encode('{"type":"error","message":"通话已结束","retry":false}\n'));
  await until(() => !f.audio.running); await delay(30);
  assert.equal(f.streams.length,1); assert.equal(f.track.readyState,'ended');
});
test('manual stop during a pending reconnect cancels retries and releases the microphone', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); let release;
  f.audio.pause = () => new Promise(resolve => { release = resolve; });
  f.audio.start('card3','stereo',{media:f.media}); await until(() => f.posts.length > 0);
  f.streams[0].controller.close(); await until(() => release);
  f.audio.stop(); release(); await delay(30);
  assert.equal(f.streams.length,1); assert.equal(f.track.readyState,'ended'); assert.equal(f.audio.running,false);
});
test('microphone removal stops capture and reception', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3','stereo',{media:f.media});
  await until(() => f.posts.length > 0); f.track.dispatchEvent(new Event('ended')); await delay(20);
  assert.equal(f.audio.running,false); assert.equal(f.streams.length,1);
});
test('a muted track without input channels continues sending silence', () => {
  let Processor;
  const output = [];
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../web/microphone-worklet.js'), 'utf8'), {
    AudioWorkletProcessor: class { port = {postMessage: data => output.push(data)}; },
    registerProcessor: (_, ctor) => { Processor = ctor; }, sampleRate: 48000
  });
  const processor = new Processor();
  for (let i = 0; i < 100; i++) assert.equal(processor.process([[]]), true);
  assert.equal(output.length, 13);
  for (const data of output) {
    assert.equal(data.byteLength,160); assert.equal(new Uint8Array(data).some(v => v !== 0),false);
  }
});

test('a browser that prunes the muted source graph still uploads silence after reconnect', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3','stereo',{media:f.media});
  await until(() => f.posts.length > 0); f.audio.toggleMute(); f.setRendering(false);
  f.streams[0].controller.close();
  await until(() => f.posts.some(p => p.session === 'session1'));
  const post = f.posts.find(p => p.session === 'session1');
  assert.equal(Buffer.from(post.data,'base64').some(v => v !== 0),false);
  assert.equal(f.track.enabled,false);
  f.audio.stop(); const count = f.posts.length; await delay(250);
  assert.equal(f.posts.length,count);
});
test('capture stalls without muting keep uploading silent packets, then resume speech', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3','stereo',{media:f.media});
  await until(() => f.posts.length > 0); f.setRendering(false);
  const before = f.posts.length;
  await until(() => f.posts.slice(before).some(p => !Buffer.from(p.data,'base64').some(v => v !== 0)));
  const silentAt = f.posts.length;
  await until(() => f.posts.length > silentAt + 2);
  assert.equal(f.audio.muted,false); assert.equal(f.streams.length,1);
  f.setRendering(true); const resumedAt = f.posts.length;
  await until(() => f.posts.slice(resumedAt).some(p => Buffer.from(p.data,'base64').some(v => v !== 0)));
  f.audio.stop(); const endedAt = f.posts.length; await delay(250);
  assert.equal(f.posts.length,endedAt);
});
test('audio keeps recovering beyond eight interruptions until the call ends', async t => {
  const f = fixture(); t.after(() => f.audio.stop()); f.audio.start('card3','stereo',{media:f.media});
  for(let i=0;i<10;i++) {
    await until(() => f.posts.some(p => p.session === `session${i}`));
    f.streams[i].controller.enqueue(new TextEncoder().encode('{"type":"error","message":"temporary interruption","retry":true}\n'));
  }
  await until(() => f.posts.some(p => p.session === 'session10'));
  assert.equal(f.audio.running,true); assert.equal(f.track.readyState,'live');
  f.streams[10].controller.enqueue(new TextEncoder().encode('{"type":"error","message":"通话已结束","retry":false}\n'));
  await until(() => !f.audio.running);
  assert.equal(f.track.readyState,'ended');
});
