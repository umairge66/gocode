package mods

// kitJS is 'gocode/testing', the kit a mod's *.test.ts files import under
// `gocode plugin test`: test / describe / tier / expect / mock, and the run
// the host calls once the file has loaded. A test body gets `$` (the engine's
// own: every call raises its event through the plugins, then through the
// test's own hooks, which `on` registers beneath every plugin) and `on`.
//
// The kit is condensed into one module; the host side of its handover (open, load,
// call, close, now, settle) is testkit.go.
const kitJS = `
const isObject = (value) => value !== null && typeof value === 'object';
const isDuration = (ms) => typeof ms === 'number' && ms >= 0 && ms < Number.POSITIVE_INFINITY;
const isIterable = (value) => value !== null && value !== undefined && typeof Reflect.get(Object(value), Symbol.iterator) === 'function';
export const PLUGIN_TIERS = ['prepend', 'user', 'append', 'builtin'];
export const PLUGIN_TIERS_TEXT = "'prepend', 'user', 'append' or 'builtin'";
const isPluginTier = (value) => PLUGIN_TIERS.some((tier) => tier === value);
const DELAY_MAX_MS = 2147483647;
const TIMEOUT_MS = 5000;
const FIRES_PER_ADVANCE_MAX = 10000;

// installed
let held;
const installed = {
  install: (handover) => { if (held) throw new TypeError('the kit is installed once'); held = handover; },
  handover: () => { if (!held) throw new TypeError("'gocode/testing' runs under gocode plugin test; nothing here installed it"); return held; },
};
export function install(host, eventsText) { installed.install({ host, events: JSON.parse(eventsText) }); }

// collector
function titleOf(group, name) {
  const names = [name];
  for (let at = group; at; at = at.parent) if (at.name !== '') names.unshift(at.name);
  return names.join(' > ');
}
function testCaseOf(title, rest) {
  const [options, body] = rest.length === 1 ? [{}, rest[0]] : rest;
  if (typeof body !== 'function') throw new TypeError('test(name, body) or test(name, options, body): a string, then a function ($, on)');
  if (!isObject(options)) throw new TypeError('test(name, options, body): options is an object');
  const { plugins = [], timeoutMs = TIMEOUT_MS, options: values } = options;
  if (!Array.isArray(plugins)) throw new TypeError('test(name, { plugins }): an array of plugins');
  if (!(typeof timeoutMs === 'number' && timeoutMs > 0)) throw new TypeError('test(' + JSON.stringify(title) + ', { timeoutMs }): a positive number');
  const isValues = values === undefined || (isObject(values) && Object.prototype.toString.call(values) === '[object Object]');
  if (!isValues) throw new TypeError('test(' + JSON.stringify(title) + ", { options }): an object of the plugin's userConfig values");
  return { title, body, plugins, timeoutMs, options: values };
}
function createCollector() {
  const root = { name: '', parent: null };
  const cases = [];
  let current = root;
  let isCollecting = true;
  let fileTier;
  const collecting = (what) => { if (!isCollecting) throw new TypeError(what + ' is called while the file loads, not inside a test'); };
  return {
    describe: (name, body) => {
      collecting('describe');
      if (!(typeof name === 'string' && typeof body === 'function')) throw new TypeError('describe(name, body) takes a string and a function');
      const outer = current;
      current = { name, parent: outer };
      try { body(); } finally { current = outer; }
    },
    test: (name, rest) => {
      collecting('test');
      if (typeof name !== 'string') throw new TypeError('test(name, ...): the name is a string');
      cases.push(testCaseOf(titleOf(current, name), rest));
    },
    tier: (tier) => {
      collecting('tier(tier)');
      if (fileTier) throw new TypeError('tier(tier) is called once, for the whole file');
      if (!isPluginTier(tier)) throw new TypeError('tier(' + JSON.stringify(tier) + '): the tier is ' + PLUGIN_TIERS_TEXT);
      fileTier = tier;
    },
    close: () => { isCollecting = false; },
    cases: () => cases,
    fileTier: () => fileTier ?? 'user',
  };
}
const collector = createCollector();
export const describe = (name, body) => collector.describe(name, body);
export const test = (name, ...rest) => collector.test(name, rest);
export const tier = (t) => collector.tier(t);

// equality
const probes = new WeakSet();
const asymmetry = {
  asymmetric: (text, t) => { const probe = Object.freeze({ text, test: t }); probes.add(probe); return probe; },
  isAsymmetric: (value) => isObject(value) && probes.has(value),
};
const keysOf = (sameness, value) => Object.keys(value).filter((key) => sameness.mode === 'strict' || value[key] !== undefined);
function propertiesEqual(sameness, actual, expected) {
  if (sameness.mode === 'subset') return Object.keys(expected).every((key) => key in actual && sameness.same(actual[key], expected[key]));
  const expectedKeys = keysOf(sameness, expected);
  return keysOf(sameness, actual).length === expectedKeys.length && expectedKeys.every((key) => Object.prototype.hasOwnProperty.call(actual, key) && sameness.same(actual[key], expected[key]));
}
const arraysEqual = (s, a, e) => a.length === e.length && e.every((value, at) => s.same(a[at], value));
const mapsEqual = (s, a, e) => a instanceof Map && e instanceof Map && a.size === e.size && [...e].every(([k, v]) => a.has(k) && s.same(a.get(k), v));
const setsEqual = (s, a, e) => a instanceof Set && e instanceof Set && a.size === e.size && [...e].every((v) => [...a].some((c) => s.same(c, v)));
function structurallyEqual(sameness, actual, expected) {
  const isOtherPrototype = sameness.mode === 'strict' && Object.getPrototypeOf(actual) !== Object.getPrototypeOf(expected);
  if (isOtherPrototype || Array.isArray(actual) !== Array.isArray(expected)) return false;
  if (actual instanceof Date || expected instanceof Date) return actual instanceof Date && expected instanceof Date && Object.is(actual.getTime(), expected.getTime());
  if (actual instanceof RegExp || expected instanceof RegExp) return String(actual) === String(expected);
  if (actual instanceof Map || expected instanceof Map) return mapsEqual(sameness, actual, expected);
  if (actual instanceof Set || expected instanceof Set) return setsEqual(sameness, actual, expected);
  return Array.isArray(actual) && Array.isArray(expected) ? arraysEqual(sameness, actual, expected) : propertiesEqual(sameness, actual, expected);
}
function equalsUnder(comparison, actual, expected) {
  if (asymmetry.isAsymmetric(expected)) return expected.test(actual);
  if (Object.is(actual, expected)) return true;
  if (!isObject(actual) || !isObject(expected)) return false;
  if (comparison.seen.some(([one, other]) => one === actual && other === expected)) return true;
  const inner = { mode: comparison.mode, seen: [...comparison.seen, [actual, expected]] };
  return structurallyEqual({ mode: comparison.mode, same: (one, other) => equalsUnder(inner, one, other) }, actual, expected);
}
const equals = (actual, expected, mode) => equalsUnder({ mode, seen: [] }, actual, expected);
const FORMAT_WIDTH = 60;
const keyText = (key) => /^[A-Za-z_$][\w$]*$/.test(key) ? key : JSON.stringify(key);
function partsOf(value, shown) {
  if (Array.isArray(value)) return value.map(shown);
  if (value instanceof Map) return [...value].map(([key, part]) => shown(key) + ' => ' + shown(part));
  return value instanceof Set ? [...value].map(shown) : Object.keys(value).map((key) => keyText(key) + ': ' + shown(value[key]));
}
function bracketsOf(value) {
  if (Array.isArray(value)) return ['[', ']'];
  const name = value instanceof Map ? 'Map ' : value instanceof Set ? 'Set ' : '';
  return [name + '{', '}'];
}
function containerText(value, indent, shown) {
  const parts = partsOf(value, shown);
  const [open, close] = bracketsOf(value);
  if (parts.length === 0) return open + close;
  const pad = open === '[' ? '' : ' ';
  const flat = open + pad + parts.join(', ') + pad + close;
  if (flat.length <= FORMAT_WIDTH && !flat.includes('\n')) return flat;
  return open + '\n' + parts.map((part) => indent + '  ' + part).join(',\n') + '\n' + indent + close;
}
const dateText = (date) => { const time = date.getTime(); return Number.isNaN(time) ? 'Date(Invalid)' : 'Date(' + time + ')'; };
export function format(value, indent = '', seen = []) {
  if (typeof value === 'string') return JSON.stringify(value);
  if (typeof value === 'bigint') return value + 'n';
  if (typeof value === 'symbol') return value.toString();
  if (typeof value === 'function') return value.name === '' ? '[Function]' : '[Function ' + value.name + ']';
  if (!isObject(value)) return String(value);
  if (asymmetry.isAsymmetric(value)) return value.text;
  if (seen.includes(value)) return '[Circular]';
  if (value instanceof Error) return value.name + ': ' + value.message;
  if (value instanceof RegExp) return String(value);
  return value instanceof Date ? dateText(value) : containerText(value, indent, (part) => format(part, indent + '  ', [...seen, value]));
}

// expect
const ASSERTION_ERROR_NAME = 'AssertionError';
function assertionErrorOf(text) { const error = new Error(text); error.name = ASSERTION_ERROR_NAME; return error; }
const leadOf = (message) => message === undefined ? '' : message + '\n\n';
function isAffixed(received, affix, end) {
  if (!(typeof received === 'string' && typeof affix === 'string')) return false;
  return end === 'start' ? received.startsWith(affix) : received.endsWith(affix);
}
const isMatched = (received, pattern) => pattern instanceof RegExp ? pattern.test(received) : received.includes(String(pattern));
function isThrownAs(thrown, expected) {
  if (!thrown) return false;
  if (expected === undefined) return true;
  const { error } = thrown;
  const message = String(isObject(error) ? error.message : error);
  if (typeof expected === 'string') return message.includes(expected);
  if (expected instanceof RegExp) return expected.test(message);
  return typeof expected === 'function' ? error instanceof expected : isObject(expected) && message === expected.message;
}
const lengthOf = (received) => received === null || received === undefined ? received : Reflect.get(Object(received), 'length');
const orderedOf = (value) => typeof value === 'bigint' || typeof value === 'number' ? value : Number(value);
function propertyAt(received, path) {
  const keys = Array.isArray(path) ? path : String(path).split('.');
  let at = received;
  for (const key of keys) {
    if (at === null || at === undefined || !(String(key) in Object(at))) return { isFound: false, value: undefined };
    at = Reflect.get(Object(at), String(key));
  }
  return { isFound: true, value: at };
}
function thrownBy(received, mode) {
  if (mode === 'rejected') return { error: received };
  if (typeof received !== 'function') throw assertionErrorOf('expect(received).toThrow(): received is not a function');
  try { received(); } catch (error) { return { error }; }
  return undefined;
}
const MATCHER_TABLE = {
  toBe: (r, [e]) => ({ isPassing: Object.is(r, e), expected: format(e) }),
  toEqual: (r, [e]) => ({ isPassing: equals(r, e, 'equal'), expected: format(e) }),
  toStrictEqual: (r, [e]) => ({ isPassing: equals(r, e, 'strict'), expected: format(e) }),
  toMatchObject: (r, [e]) => ({ isPassing: isObject(r) && equals(r, e, 'subset'), expected: format(e) }),
  toContain: (r, [item]) => ({ isPassing: typeof r === 'string' ? typeof item === 'string' && r.includes(item) : isIterable(r) && [...r].includes(item), expected: 'containing ' + format(item) }),
  toContainEqual: (r, [item]) => ({ isPassing: isIterable(r) && [...r].some((part) => equals(part, item, 'equal')), expected: 'containing ' + format(item) }),
  toHaveLength: (r, [length]) => ({ isPassing: r !== null && r !== undefined && lengthOf(r) === length, expected: 'length ' + String(length), shown: 'length ' + String(lengthOf(r)) }),
  toHaveProperty: (r, args) => {
    const [path, value] = args;
    const found = propertyAt(r, path);
    const isValued = args.length > 1;
    return { isPassing: found.isFound && (!isValued || equals(found.value, value, 'equal')), expected: 'property ' + format(path) + (isValued ? ' = ' + format(value) : '') };
  },
  toBeUndefined: (r) => ({ isPassing: r === undefined, expected: 'undefined' }),
  toBeDefined: (r) => ({ isPassing: r !== undefined, expected: 'defined' }),
  toBeNull: (r) => ({ isPassing: r === null, expected: 'null' }),
  toBeTruthy: (r) => ({ isPassing: Boolean(r), expected: 'truthy' }),
  toBeFalsy: (r) => ({ isPassing: !r, expected: 'falsy' }),
  toBeNaN: (r) => ({ isPassing: typeof r === 'number' && Number.isNaN(r), expected: 'NaN' }),
  toBeGreaterThan: (r, [b]) => ({ isPassing: orderedOf(r) > orderedOf(b), expected: '> ' + format(b) }),
  toBeGreaterThanOrEqual: (r, [b]) => ({ isPassing: orderedOf(r) >= orderedOf(b), expected: '>= ' + format(b) }),
  toBeLessThan: (r, [b]) => ({ isPassing: orderedOf(r) < orderedOf(b), expected: '< ' + format(b) }),
  toBeLessThanOrEqual: (r, [b]) => ({ isPassing: orderedOf(r) <= orderedOf(b), expected: '<= ' + format(b) }),
  toMatch: (r, [p]) => ({ isPassing: typeof r === 'string' && isMatched(r, p), expected: format(p) }),
  toStartWith: (r, [p]) => ({ isPassing: isAffixed(r, p, 'start'), expected: 'starting with ' + format(p) }),
  toEndWith: (r, [s]) => ({ isPassing: isAffixed(r, s, 'end'), expected: 'ending with ' + format(s) }),
  toBeInstanceOf: (r, [e]) => { const isClass = typeof e === 'function'; return { isPassing: isClass && r instanceof e, expected: 'an instance of ' + (isClass ? e.name : format(e)) }; },
  toThrow: (r, [e], mode) => { const thrown = thrownBy(r, mode); return { isPassing: isThrownAs(thrown, e), expected: e === undefined ? 'a throw' : format(e), shown: thrown ? format(thrown.error) : 'nothing thrown' }; },
};
const MATCHER_NAMES = Object.keys(MATCHER_TABLE);
const byMatcher = (make) => Object.fromEntries(MATCHER_NAMES.map((name) => [name, make(name)]));
function check(context, name, args) {
  const { received, polarity, mode, message, via } = context;
  const verdict = MATCHER_TABLE[name](received, args, mode);
  const isNegated = polarity === 'negated';
  if (verdict.isPassing !== isNegated) return;
  throw assertionErrorOf(leadOf(message) + 'expect(received).' + via + (isNegated ? 'not.' : '') + name + '()\n\nExpected: ' + (isNegated ? 'not ' : '') + verdict.expected + '\nReceived: ' + (verdict.shown ?? format(received)));
}
const checksOf = (context) => byMatcher((name) => (...args) => { check(context, name, args); });
async function outcomeOf(received) {
  try { return { value: await received, isRejected: false }; } catch (error) { return { value: error, isRejected: true }; }
}
const settledChecksOf = (context, settlement) => byMatcher((name) => async (...args) => {
  const outcome = await outcomeOf(context.received);
  if (outcome.isRejected !== (settlement === 'rejects')) {
    const way = outcome.isRejected ? 'rejected' : 'resolved';
    throw assertionErrorOf(leadOf(context.message) + 'expect(received).' + settlement + '.' + name + '()\n\nReceived promise ' + way + ' with ' + format(outcome.value));
  }
  check({ ...context, received: outcome.value, mode: settlement === 'rejects' ? 'rejected' : 'value', via: settlement + '.' }, name, args);
});
const negatableOf = (checks, negated) => ({ ...checks, not: negated });
function expectationOf(received, message) {
  const plain = { received, polarity: 'plain', mode: 'value', message, via: '' };
  const negated = { ...plain, polarity: 'negated' };
  return {
    ...negatableOf(checksOf(plain), checksOf(negated)),
    resolves: negatableOf(settledChecksOf(plain, 'resolves'), settledChecksOf(negated, 'resolves')),
    rejects: negatableOf(settledChecksOf(plain, 'rejects'), settledChecksOf(negated, 'rejects')),
  };
}
const MATCHING = {
  any: (expected) => asymmetry.asymmetric('Any<' + expected.name + '>', (value) => value !== null && value !== undefined && Object(value) instanceof expected),
  anything: () => asymmetry.asymmetric('Anything', (value) => value !== null && value !== undefined),
  stringContaining: (text) => asymmetry.asymmetric('StringContaining ' + format(text), (value) => typeof value === 'string' && value.includes(text)),
  stringMatching: (pattern) => asymmetry.asymmetric('StringMatching ' + format(pattern), (value) => typeof value === 'string' && new RegExp(pattern).test(value)),
  objectContaining: (shape) => asymmetry.asymmetric('ObjectContaining ' + format(shape), (value) => isObject(value) && equals(value, shape, 'subset')),
  arrayContaining: (items) => asymmetry.asymmetric('ArrayContaining ' + format(items), (value) => Array.isArray(value) && items.every((item) => value.some((part) => equals(part, item, 'equal')))),
};
export const expect = Object.assign((received, message) => expectationOf(received, message), MATCHING);

// mock
function createPendingClock(start) {
  const pending = new Set();
  let now = start;
  let order = 0;
  return {
    now: () => now,
    moveTo: (ms) => { now = ms; },
    wait: (ms, settling) => { order += 1; const wait = { ...settling, at: now + ms, order }; pending.add(wait); return wait; },
    drop: (wait) => { pending.delete(wait); },
    due: (until) => [...pending].filter((w) => w.at <= until).sort((a, b) => a.at - b.at || a.order - b.order)[0],
  };
}
async function advanceClock(clock, ms) {
  if (!isDuration(ms)) throw new TypeError('advance(ms): a finite, non-negative number of milliseconds');
  const { settle } = installed.handover().host;
  const until = clock.now() + ms;
  await settle();
  let fired = 0;
  for (let wait = clock.due(until); wait; wait = clock.due(until)) {
    if (fired >= FIRES_PER_ADVANCE_MAX) throw new Error('advance(ms): ' + FIRES_PER_ADVANCE_MAX + ' waits resolved and more were due: an interval shorter than the advance?');
    fired += 1;
    clock.drop(wait);
    clock.moveTo(wait.at);
    wait.resolve();
    await settle();
  }
  clock.moveTo(until);
  await settle();
}
const holdOf = (clock) => ($, e, next) => new Promise((resolve, reject) => {
  const wait = clock.wait(e.ms, { resolve: () => { resolve({ value: undefined }); }, reject });
  function cancel() { clock.drop(wait); reject(next.signal.reason); }
  next.signal.aborted ? cancel() : next.signal.addEventListener('abort', cancel, { once: true });
});
function startOf(options) {
  const start = isObject(options) ? options.now : undefined;
  const isStart = isObject(options) && (start === undefined || (typeof start === 'number' && Number.isFinite(start)));
  if (!isStart) throw new TypeError('mock.clock(on, { now }): the on of a test, and the time it starts at in milliseconds (0 when unsaid)');
  return typeof start === 'number' ? start : 0;
}
function mockClock(on, options = {}) {
  const clock = createPendingClock(startOf(options));
  const hold = holdOf(clock);
  on('clock.now', () => ({ value: clock.now() }));
  on('clock.sleep', hold);
  on('clock.after', hold);
  on('clock.every', hold);
  return Object.freeze({
    now: clock.now,
    advance: (ms) => advanceClock(clock, ms),
    set: (ms) => {
      if (!(typeof ms === 'number' && Number.isFinite(ms) && ms >= clock.now())) throw new TypeError('set(ms): a time in milliseconds at or past now (' + clock.now() + '); the clock does not run backwards');
      return advanceClock(clock, ms - clock.now());
    },
    settle: () => advanceClock(clock, 0),
    sleep: (ms) => {
      if (!isDuration(ms)) throw new TypeError('sleep(ms): a finite, non-negative number of milliseconds');
      return new Promise((resolve) => { clock.wait(ms, { resolve, reject: () => undefined }); });
    },
  });
}
function mockEnv(on, variables) {
  if (!isObject(variables)) throw new TypeError('mock.env(on, variables): an object of strings');
  const held = { ...variables };
  on('env.get', ($, e) => { const value = held[e.name]; return { value: typeof value === 'string' ? value : undefined }; });
}
function mockSession(on) {
  let appended = [];
  on('session.append', async ($, e, next) => {
    const stored = await next(e);
    appended = stored.deny === undefined ? [...appended, { ...e, message: stored.message }] : appended;
    return stored;
  });
  return { appended: () => appended };
}
function mockStore(on, entries = {}) {
  if (!isObject(entries)) throw new TypeError('mock.store(on, entries): an object of values');
  const held = new Map(Object.entries(entries));
  on('store.get', ($, e) => ({ value: held.get(e.key) }));
  on('store.set', ($, e) => { held.set(e.key, e.value); return { value: undefined }; });
  on('store.delete', ($, e) => { held.delete(e.key); return { value: undefined }; });
  on('store.keys', () => ({ value: [...held.keys()] }));
}
export const mock = { clock: mockClock, store: mockStore, env: mockEnv, session: mockSession };

// open-seat
const isPlainHook = (hook) => typeof hook === 'function' && Object.prototype.toString.call(hook) !== '[object AsyncGeneratorFunction]';
function createChecks() {
  let first;
  return {
    note: (error, event) => { const isCheck = isObject(error) && error.name === ASSERTION_ERROR_NAME; first ??= isCheck ? { text: errorTextOf(error), event } : undefined; },
    first: () => first,
  };
}
function checkedHookOf(hook, event, checks) {
  return isPlainHook(hook) ? async function checked($, e, next) {
    try { return await hook($, e, next); } catch (error) { checks.note(error, event); throw error; }
  } : hook;
}
function helperSpecOf(plugin) {
  const fields = isObject(plugin) ? plugin : {};
  const { name, register, tier = 'user' } = fields;
  if (typeof name !== 'string' || typeof register !== 'function') throw new TypeError('test(name, { plugins }): each is { name, tier?, register(on) { ... } }');
  if (!isPluginTier(tier)) throw new TypeError('test(name, { plugins }): ' + name + "'s tier is " + PLUGIN_TIERS_TEXT);
  return { name, tier, source: Function.prototype.toString.call(register) };
}
const callOf = (crossing, event) => async (e) => { await crossing.load(); return crossing.call(event, e === undefined ? {} : e); };
function engineOf(crossing) {
  return Object.freeze(Object.fromEntries(installed.handover().events.map(([noun, events]) => [noun, Object.freeze(Object.fromEntries(events.map(([name]) => [name, callOf(crossing, noun + '.' + name)])))])));
}
function openSeat(plugins, tierName, options) {
  const host = installed.handover().host;
  const clauses = [];
  const checks = createChecks();
  const patterns = new Set();
  const id = host.open(JSON.stringify({ tier: tierName, plugins: plugins.map(helperSpecOf), options }));
  let loading;
  function on(pattern, ...rest) {
    if (loading) throw new TypeError('on("' + String(pattern) + '") after the test first called $: the hooks beneath the plugins are registered before that, as a module registers its hooks in register()');
    if (!(typeof pattern === 'string' && rest.length >= 1 && rest.length <= 2)) throw new TypeError('on(event, hook) or on(event, matcher, hook)');
    const clause = { pattern, rest: [...rest.slice(0, -1), checkedHookOf(rest[rest.length - 1], pattern, checks)], caught: undefined };
    clauses.push(clause);
    patterns.add(clause.pattern);
    return Object.freeze({ catch: (handler) => { clause.caught = checkedHookOf(handler, clause.pattern, checks); } });
  }
  function load() {
    loading ??= host.load(id, JSON.stringify([...patterns]), {
      register: (engineOn) => {
        for (const clause of clauses) {
          const registration = engineOn(clause.pattern, ...clause.rest);
          if (clause.caught !== undefined) registration.catch(clause.caught);
        }
      },
    });
    return loading;
  }
  return {
    $: engineOf({ load, call: (event, e) => host.call(id, event, e) }),
    on,
    close: async () => {
      const end = await host.close(id);
      const failed = checks.first() ?? end.failed;
      return failed === undefined ? end : { ...end, failed };
    },
  };
}

// run
function readOf(error, key) { if (!isObject(error)) return undefined; try { return error[key]; } catch (e) { return undefined; } }
function errorTextOf(error) {
  const stack = readOf(error, 'stack');
  const message = readOf(error, 'message');
  if (typeof stack === 'string' && stack !== '') {
    return typeof message === 'string' && !stack.startsWith((readOf(error, 'name') ?? 'Error') + ':') ? (readOf(error, 'name') ?? 'Error') + ': ' + message + '\n' + stack : stack;
  }
  if (typeof message === 'string') return message;
  try { return String(error); } catch (e) { return 'threw a value that does not print'; }
}
function hintOf(message) {
  const met = /no implementation for ([\w.*-]+)/.exec(message);
  return [
    ...(met ? ['nothing beneath the plugins answers ' + met[1] + ": a test answers it with on('" + met[1] + "', ...): { value: ... } for a call on $, the event's own result otherwise"] : []),
    ...(met?.[1] === 'ui.render' ? ["nothing draws it beneath the plugins: the test's own on('ui.render', ...) stands for the engine's drawing"] : []),
    ...(message.includes('neither { value } nor { deny }') ? ['a stand-in for a call on $ answers { value: ... }, what it resolves to'] : []),
    ...(message.includes('something that is not a result object') ? ["a hook answers a result object: the event's own result, or { value: ... } for a call on $"] : []),
  ];
}
function failureOf(text, told) {
  const lines = text.split('\n');
  const stackAt = lines.findIndex((line) => /^\s+at /.test(line));
  const message = stackAt === -1 ? lines : lines.slice(0, stackAt);
  const frames = stackAt === -1 ? [] : lines.slice(stackAt);
  const notes = [...hintOf([...message, told].join('\n')), ...(told === '' ? [] : [told])];
  return [message.join('\n'), ...notes.map((note) => '\n' + note), ...(frames.length === 0 ? [] : ['\n' + frames.join('\n')])].join('\n');
}
const hookNotesOf = (event, told) => ["in the test's " + event + ' hook', ...(told === '' ? [] : [told])].join('\n\n');
const overranError = (timeoutMs) => new Error('timed out after ' + timeoutMs + ' ms');
function timed(body, timeoutMs) {
  const { now } = installed.handover().host;
  return new Promise((resolve, reject) => {
    const started = now();
    const timer = setTimeout(() => reject(overranError(timeoutMs)), Math.min(timeoutMs, DELAY_MAX_MS));
    Promise.resolve().then(body).then((value) => {
      clearTimeout(timer);
      now() - started > timeoutMs ? reject(overranError(timeoutMs)) : resolve(value);
    }, (error) => { clearTimeout(timer); reject(error); });
  });
}
async function runCase(one) {
  const { now } = installed.handover().host;
  const started = now();
  let thrown;
  let seat;
  try {
    seat = openSeat(one.plugins, collector.fileTier(), one.options);
    const { $, on } = seat;
    await timed(() => one.body($, on), one.timeoutMs);
  } catch (error) {
    thrown = errorTextOf(error);
  }
  let end = { told: '' };
  try { end = seat ? await seat.close() : end; } catch (error) { thrown ??= errorTextOf(error); }
  const { told, failed } = end;
  const failure = thrown ?? failed?.text;
  const isHooks = thrown === undefined && failed !== undefined;
  const notes = isHooks ? hookNotesOf(failed.event, told) : told;
  return { title: one.title, ms: now() - started, failure: failure === undefined ? null : failureOf(failure, notes) };
}
export async function run() {
  collector.close();
  const results = [];
  for (const one of collector.cases()) results.push(await runCase(one));
  return JSON.stringify(results);
}
`
