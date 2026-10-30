package mods

// preludeJS sets up a hooks environment before the module runs: the web APIs
// a module may use (timers, console, AbortController, structuredClone), the
// JSX globals `h` and `Fragment`, and the engine's internals, which the
// prelude returns to the host and never leaves on the global object.
//
// Values cross between the host and an environment as JSON text, frozen to
// every depth on arrival (anything else "is not plain data", and
// `e` is frozen to every depth).
const preludeJS = `(function (g, host) {
'use strict';
const goTimer = host.timer, goClear = host.clearTimer, goLog = host.log;
g.setTimeout = (fn, ms, ...args) => goTimer(Math.max(0, Number(ms) || 0), false, () => fn(...args));
g.setInterval = (fn, ms, ...args) => goTimer(Math.max(1, Number(ms) || 0), true, () => fn(...args));
g.clearTimeout = (id) => { if (typeof id === 'number') goClear(id); };
g.clearInterval = g.clearTimeout;
g.queueMicrotask = (fn) => { Promise.resolve().then(fn); };
const show = (x) => { if (typeof x === 'string') return x; try { return JSON.stringify(x); } catch (e) { return String(x); } };
const con = {};
for (const k of ['log', 'info', 'warn', 'error', 'debug']) con[k] = (...a) => goLog(k, a.map(show).join(' '));
g.console = Object.freeze(con);

class AbortSignal {
  constructor() { this.aborted = false; this.reason = undefined; this.onabort = null; Object.defineProperty(this, '_l', { value: [], writable: true }); }
  addEventListener(type, fn, opts) { if (type === 'abort' && typeof fn === 'function') this._l.push(fn); }
  removeEventListener(type, fn) { this._l = this._l.filter((f) => f !== fn); }
  throwIfAborted() { if (this.aborted) throw this.reason; }
  static abort(reason) { const s = new AbortSignal(); abortSignal(s, reason); return s; }
  static timeout(ms) { const s = new AbortSignal(); g.setTimeout(() => abortSignal(s, new Error('The operation timed out')), ms); return s; }
}
function abortSignal(s, reason) {
  if (s.aborted) return;
  s.aborted = true;
  s.reason = reason === undefined ? new Error('This operation was aborted') : reason;
  const ev = { type: 'abort', target: s };
  if (typeof s.onabort === 'function') { try { s.onabort(ev); } catch (e) {} }
  const ls = s._l; s._l = [];
  for (const f of ls) { try { f.call(s, ev); } catch (e) {} }
}
class AbortController {
  constructor() { this.signal = new AbortSignal(); }
  abort(reason) { abortSignal(this.signal, reason); }
}
g.AbortSignal = AbortSignal;
g.AbortController = AbortController;
g.structuredClone = (v) => v === undefined ? undefined : JSON.parse(JSON.stringify(v));
g.Fragment = 'Fragment';
g.h = (type, props, ...children) => ({ type: typeof type === 'function' ? (type.name || 'Component') : type, props: props == null ? {} : props, children: children.flat(Infinity) });

const deepFreeze = (v) => {
  if (v !== null && typeof v === 'object' && !Object.isFrozen(v)) {
    Object.freeze(v);
    for (const k of Object.keys(v)) deepFreeze(v[k]);
  }
  return v;
};
const UNDEF = '\u0000gocode:undefined';
const fromPlain = (s) => s === undefined || s === '' ? undefined : deepFreeze(JSON.parse(s, (k, x) => x === UNDEF ? undefined : x));
const toPlain = (v) => {
  const seen = [];
  const check = (x, at) => {
    const t = typeof x;
    if (t === 'function' || t === 'symbol' || t === 'bigint') throw new TypeError('is not plain data (' + t + ' at ' + at + ')');
    if (x !== null && t === 'object') {
      if (seen.includes(x)) throw new TypeError('is not plain data (a cycle at ' + at + ')');
      seen.push(x);
      for (const k of Object.keys(x)) check(x[k], at + '.' + k);
      seen.pop();
    }
  };
  check(v, 'e');
  return v === undefined ? undefined : JSON.stringify(v, (k, x) => x === undefined && k !== '' ? UNDEF : x);
};
const errorText = (err) => {
  try {
    if (err !== null && typeof err === 'object') {
      if (typeof err.stack === 'string' && err.stack !== '') return String(err.message === undefined ? err.stack : (err.name || 'Error') + ': ' + err.message + '\n' + err.stack.split('\n').filter((l) => /^\s+at /.test(l)).join('\n'));
      if (typeof err.message === 'string') return err.message;
    }
    return String(err);
  } catch (e) { return 'threw a value that does not print'; }
};
const errorMessage = (err) => {
  try { return err !== null && typeof err === 'object' && typeof err.message === 'string' ? err.message : String(err); }
  catch (e) { return 'threw a value that does not print'; }
};

const matchOne = (m, v) => {
  if (Array.isArray(v)) return v.some((x) => matchOne(m, x));
  if (m instanceof RegExp) { m.lastIndex = 0; return v !== undefined && v !== null && m.test(String(v)); }
  if (m !== null && typeof m === 'object') return v !== null && typeof v === 'object' && Object.keys(m).every((k) => matchValue(m[k], v[k]));
  return m === v;
};
const matchValue = (m, v) => Array.isArray(m) ? m.some((x) => matchOne(x, v)) : matchOne(m, v);
const matches = (matcher, e) => matcher === undefined || (matcher !== null && typeof matcher === 'object' && Object.keys(matcher).every((k) => matchValue(matcher[k], e === null || e === undefined ? undefined : e[k])));

const isHookFn = (f) => typeof f === 'function';
const makeOn = (record, setCatch) => {
  const on = function on(pattern, ...rest) {
    if (typeof pattern !== 'string' || rest.length < 1 || rest.length > 2) throw new TypeError('on(event, hook) or on(event, matcher, hook)');
    const hook = rest[rest.length - 1];
    const matcher = rest.length === 2 ? rest[0] : undefined;
    if (!isHookFn(hook)) throw new TypeError('on("' + pattern + '", ...): the hook is a function ($, e, next)');
    if (matcher !== undefined && (matcher === null || typeof matcher !== 'object' || Array.isArray(matcher))) throw new TypeError('on("' + pattern + '", matcher, hook): the matcher is an object written in the call');
    const index = record(pattern, matcher, hook);
    return Object.freeze({
      catch: (handler) => {
        if (typeof handler !== 'function') throw new TypeError('on("' + pattern + '").catch(handler): the handler is a function ($, e, next)');
        setCatch(index, handler);
      },
    });
  };
  return Object.freeze(on);
};

const isPattern = (p, event) => host.selects(String(p), event);
const makeNext = (call, to, event, origin, signal, budget, error, trace) => {
  const next = function next(e) {
    if (arguments.length === 0) return Promise.reject(new TypeError('next() is called with the event: next(e)'));
    return call(e);
  };
  const props = {
    to: { value: (e, tier) => to(e, tier) },
    signal: { value: signal },
    event: { value: event },
    origin: { value: origin },
    is: { value: (p, e) => isPattern(p, event) },
    trace: { get: () => fromPlain(trace()) },
    budget: { get: () => fromPlain(budget()) },
  };
  if (error !== undefined) props.error = { value: error };
  Object.defineProperties(next, props);
  return Object.freeze(next);
};
const invoke = (hook, $, e, next) => {
  try { return Promise.resolve(hook($, e, next)); } catch (err) { return Promise.reject(err); }
};

// Streams (a streaming event's HookStream): an async iterator the host
// feeds; return() lets the stream go.
const makeStream = (pull, close) => {
  let finished = false;
  const it = {
    next: () => finished ? Promise.resolve({ done: true, value: undefined }) : pull().then((r) => { if (r.done) finished = true; return r; }, (err) => { finished = true; throw err; }),
    return: (v) => { if (!finished) { finished = true; close(); } return Promise.resolve({ done: true, value: v }); },
    throw: (err) => { if (!finished) { finished = true; close(); } return Promise.reject(err); },
  };
  it[Symbol.asyncIterator] = () => it;
  return Object.freeze(it);
};
const rejectedStream = (err) => makeStream(() => Promise.reject(err), () => {});
const invokeStream = (hook, $, e, next) => {
  const g = hook($, e, next);
  if (g === null || typeof g !== 'object' || typeof g.next !== 'function') throw new TypeError('returned no stream: a hook on a streaming event is an async generator (async function* ($, e, next))');
  return g;
};
const genStep = (g) => { try { return Promise.resolve(g.next()); } catch (err) { return Promise.reject(err); } };
const genReturn = (g) => { try { if (typeof g.return === 'function') Promise.resolve(g.return(undefined)).catch(() => {}); } catch (err) {} };

// The calls on $: positional arguments to the event's argument.
const shapes = {
  'fs.read': (path, o) => ({ path, as: o !== undefined && o !== null && o.as !== undefined ? o.as : 'text' }),
  'fs.write': (path, text) => ({ path, text }),
  'fs.list': (path) => ({ path: path === undefined ? '.' : path }),
  'fs.exists': (path) => ({ path }),
  'fs.stat': (path, o) => ({ path, resolve: o !== undefined && o !== null && o.resolve !== undefined ? o.resolve : false }),
  'store.get': (key) => ({ key }),
  'store.set': (key, value) => ({ key, value }),
  'store.delete': (key) => ({ key }),
  'clock.sleep': (ms) => ({ ms }),
  'http.fetch': (url, init) => init === undefined ? { url } : { url, init },
  'process.run': (argv, init) => init === undefined ? { argv } : { argv, init },
  'settings.read': (a) => a === undefined ? {} : a,
  'env.get': (name) => ({ name }),
  'env.set': (name, value) => value === undefined ? { name } : { name, value },
  'ui.toast': (text, o) => Object.assign({ text }, o || {}),
  'ui.status': (text) => ({ text }),
  'ui.log': (text, o) => ({ text, to: o && o.to !== undefined ? o.to : 'transcript' }),
  'ui.notify': (text, o) => Object.assign({ text }, o || {}),
  'ui.notice': (id, text) => ({ tool_use_id: id, text }),
  'ui.invalidate': (event) => ({ event }),
  'ui.ask': (question, options) => ({ question, options }),
  'mcp.call': (server, tool, args) => ({ server, tool, args: args === undefined ? {} : args }),
  'mcp.connect': (server) => ({ server }),
  'model.classify': (text, labels, options) => options === undefined ? { text, labels } : { text, labels, options },
  'model.fork': (request) => request,
  'state.get': (ref) => ref,
  'state.set': (ref, value, options) => Object.assign({}, ref, { value }, options && options.ifVersion !== undefined ? { ifVersion: options.ifVersion } : {}),
  'audio.play': (clip, options) => Object.assign({ clip }, options && options.shouldLoop !== undefined ? { shouldLoop: options.shouldLoop } : {}, options && options.gain !== undefined ? { gain: options.gain } : {}),
  'process.spawn': (r) => (r !== null && typeof r === 'object') ? Object.assign({ argv: Array.isArray(r.argv) ? [...r.argv] : r.argv }, r.cwd !== undefined ? { cwd: r.cwd } : {}, r.env !== undefined ? { env: r.env } : {}, r.input !== undefined ? { input: r.input } : {}) : r,
  'audio.speak': (text, options) => options && options.voice !== undefined ? { text, voice: options.voice } : { text },
};
const fireAndForget = ['ui.toast', 'ui.status', 'ui.log', 'ui.notice', 'ui.invalidate'];
// A prompt or system given as blocks crosses as its text plus the blocks.
const blockText = (b) => b.map((x) => x.text).join('');
const isBlocks = (v) => Array.isArray(v) && v.every((x) => x !== null && typeof x === 'object' && typeof x.text === 'string');
const completeShape = (o) => {
  const out = Object.assign({}, o);
  if (isBlocks(o.prompt)) { out.prompt = blockText(o.prompt); out.promptBlocks = o.prompt; }
  if (o.system) { if (isBlocks(o.system)) { out.system = blockText(o.system); out.systemBlocks = o.system; } } else { delete out.system; }
  return out;
};
const ABORTED = { isAnswered: false, reason: 'aborted', usage: { input_tokens: 0, output_tokens: 0, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 } };
const withSignal = (p, signal, onAbort) => {
  if (!signal) return p;
  return new Promise((resolve, reject) => {
    if (signal.aborted) { onAbort(resolve, reject); return; }
    signal.addEventListener('abort', () => onAbort(resolve, reject), { once: true });
    p.then(resolve, reject);
  });
};
const makeDollar = (names, op, plugin, stream) => {
  const d = { plugin: Object.freeze({ name: plugin.name, root: plugin.root }) };
  const call = (name, args, token) => op(name, args, token);
  const cancellable = (name, args, signal, onAbort) => {
    if (!signal) return call(name, args);
    if (signal.aborted) return new Promise((resolve, reject) => onAbort(resolve, reject));
    const token = host.newToken();
    signal.addEventListener('abort', () => host.cancel(token), { once: true });
    return withSignal(call(name, args, token), signal, onAbort);
  };
  for (const name of names) {
    const [noun, method] = name.split('.');
    d[noun] = d[noun] || {};
    let fn;
    if (name === 'clock.after') {
      fn = (ms, f) => { let off = false; call(name, { ms }).then(() => { if (!off) f(); }, () => {}); return Object.freeze({ cancel: () => { off = true; } }); };
    } else if (name === 'clock.every') {
      fn = (ms, f) => {
        let off = false;
        const tick = () => call(name, { ms: Math.max(1, ms) }).then(() => { if (off) return; try { f(); } catch (e) {} tick(); }, () => {});
        tick();
        return Object.freeze({ cancel: () => { off = true; } });
      };
    } else if (name === 'clock.sleep') {
      fn = (ms, o) => {
        const p = call(name, { ms });
        const s = o && o.signal;
        if (!s) return p;
        return new Promise((resolve, reject) => {
          if (s.aborted) { reject(s.reason); return; }
          s.addEventListener('abort', () => reject(s.reason), { once: true });
          p.then(resolve, reject);
        });
      };
    } else if (name === 'clock.now') {
      fn = () => call(name, {});
    } else if (name === 'process.spawn') {
      fn = (request) => stream(name, shapes[name](request));
    } else if (name === 'model.complete') {
      // An abort resolves { reason: "aborted" }.
      fn = (request, o) => {
        const signal = o && o.signal;
        const shaped = request !== null && typeof request === 'object' ? completeShape(request) : request;
        return cancellable(name, shaped, signal, (resolve) => resolve(ABORTED)).catch((err) => { if (signal && signal.aborted) return ABORTED; throw err; });
      };
    } else if (name === 'audio.play' || name === 'audio.speak') {
      fn = (first, o) => {
        const signal = o && o.signal;
        const args = shapes[name](first, o);
        return cancellable(name, args, signal, (resolve, reject) => name === 'audio.play' ? resolve(undefined) : reject(signal.reason));
      };
    } else if (fireAndForget.includes(name)) {
      const shape = shapes[name];
      fn = (...a) => { call(name, shape(...a)).catch(() => {}); };
    } else {
      const shape = shapes[name] || ((a) => a === undefined ? {} : a);
      fn = (...a) => call(name, shape(...a));
    }
    d[noun][method] = fn;
  }
  for (const k of Object.keys(d)) Object.freeze(d[k]);
  return Object.freeze(d);
};

return Object.freeze({ deepFreeze, fromPlain, toPlain, errorText, errorMessage, matches, makeOn, makeNext, invoke, makeDollar, newSignal: () => new AbortSignal(), abort: abortSignal, makeStream, rejectedStream, invokeStream, genStep, genReturn });
})`
