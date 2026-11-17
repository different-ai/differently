// The simulator's worker: runs the Differently control plane, compiled to
// WebAssembly (cmd/differently-loadsim), and speeds up its clock.
//
// The Go runtime reads time through performance.now and Date, and sleeps with
// setTimeout. This worker replaces those three, for itself only, with a
// simulated clock. The server, the scheduler and the simulated users are
// unmodified: an idle timeout of five minutes is five minutes to them, and
// five seconds to whoever watches at 60x.
//
// The simulated clock advances `scale` times faster than the wall while the
// program sleeps, and stands still while it runs. So the control plane's own
// computing takes no simulated time, and simulated time can never outrun the
// work: on a slow machine the simulation runs slower than asked, not wrong.
//
// Page -> worker: {type: 'start', config, scale} | {type: 'scale', scale} | {type: 'command', command}
// Worker -> page: {type: 'ready'} | {type: 'snapshot', snapshot} | {type: 'error', message}

const realNow = performance.now.bind(performance);
const RealDate = Date;
const realSetTimeout = self.setTimeout.bind(self);
const realClearTimeout = self.clearTimeout.bind(self);
const realSetInterval = self.setInterval.bind(self);

const epoch = RealDate.now();
let scale = 1;
let virtual = 0; // simulated milliseconds, as of the last time the program started or stopped running
let idleSince = realNow();
let depth = 0; // above zero while the program runs
const virtualNow = () => (depth > 0 ? virtual : virtual + (realNow() - idleSince) * scale);

// turn wraps every entry into the program.
const turn = (fn) => (...args) => {
  if (depth++ === 0) virtual += (realNow() - idleSince) * scale;
  try {
    return fn(...args);
  } finally {
    if (--depth === 0) idleSince = realNow();
  }
};

// Sleeps wait in simulated time, so a change of speed re-arms them.
const timers = new Map();
let nextTimer = 1;
function arm(id, timer) {
  if (scale <= 0) return;
  const fire = turn(() => {
    // A timeout can fire a moment early; the Go runtime would spin on that.
    if (virtual < timer.due) virtual = timer.due;
    timers.delete(id);
    timer.fn(...timer.args);
  });
  timer.handle = realSetTimeout(fire, Math.max(0, (timer.due - virtualNow()) / scale));
}
function setScale(next) {
  if (depth === 0) {
    virtual = virtualNow();
    idleSince = realNow();
  }
  scale = next;
  for (const [id, timer] of timers) {
    realClearTimeout(timer.handle);
    arm(id, timer);
  }
}

performance.now = virtualNow;
self.Date = class extends RealDate {
  constructor(...args) {
    if (args.length) super(...args);
    else super(epoch + virtualNow());
  }
  static now() { return epoch + virtualNow(); }
};
self.setTimeout = (fn, delay = 0, ...args) => {
  const id = nextTimer++;
  const timer = { fn, args, due: virtualNow() + delay, handle: 0 };
  timers.set(id, timer);
  arm(id, timer);
  return id;
};
self.clearTimeout = (id) => {
  const timer = timers.get(id);
  if (!timer) return;
  realClearTimeout(timer.handle);
  timers.delete(id);
};

importScripts('wasm_exec.js');

// Commands that arrive while the program loads wait for it.
let ready = false;
const early = [];
const send = turn((command) => {
  if (ready) self.differentlySimSend(JSON.stringify(command));
  else if (command.op !== 'snapshot') early.push(command);
});

self.differentlySimEmit = (kind, json) => {
  const value = JSON.parse(json);
  if (kind === 'ready') {
    ready = true;
    early.splice(0).forEach((command) => self.differentlySimSend(JSON.stringify(command)));
    postMessage({ type: 'ready' });
  } else if (kind === 'snapshot') {
    postMessage({ type: 'snapshot', snapshot: value });
  } else if (kind === 'error') {
    postMessage({ type: 'error', message: value });
  }
};

// Snapshots on the wall clock, eight a second.
realSetInterval(() => { if (scale > 0) send({ op: 'snapshot' }); }, 125);

async function start(config, initialScale) {
  self.differentlySimConfig = JSON.stringify(config);
  const go = new Go();
  const response = fetch('differently.wasm');
  let instance;
  try {
    ({ instance } = await WebAssembly.instantiateStreaming(response, go.importObject));
  } catch {
    // A server that does not label .wasm files: compile from bytes instead.
    const bytes = await (await fetch('differently.wasm')).arrayBuffer();
    ({ instance } = await WebAssembly.instantiate(bytes, go.importObject));
  }
  setScale(initialScale);
  await turn(() => go.run(instance))();
  postMessage({ type: 'error', message: 'the simulator stopped' });
}

self.onmessage = ({ data }) => {
  if (data.type === 'start') {
    start(data.config, data.scale).catch((err) => postMessage({ type: 'error', message: String(err && err.message || err) }));
  } else if (data.type === 'scale') {
    setScale(data.scale);
    send({ op: 'snapshot' });
  } else if (data.type === 'command') {
    send(data.command);
    send({ op: 'snapshot' });
  }
};
