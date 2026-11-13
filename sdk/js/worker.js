import { Buffer } from "node:buffer";
import { TextDecoder } from "node:util";
import { MAX_CHUNK_BYTES, MAX_CONCURRENT, MAX_FRAME_BYTES, MAX_HOST_BYTES, SDKError, decodeBytes } from "./bytes.js";
import { freezeJSON, parseJSON, publicManifest, stringifyJSON, validateModule } from "./manifest.js";
import { ComponentLifecycle } from "./lifecycle.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const identifier = value => typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 256 && !/[\s\p{Cc}]/u.test(value);
const uint64 = value => typeof value === "number" && Number.isSafeInteger(value) && value >= 0
  || typeof value === "bigint" && value >= 0n && value <= 18446744073709551615n;
const invalid = () => new SDKError("invalid", "Invalid RPC frame");
const canceled = () => new SDKError("canceled", "Call canceled");
const deadline = () => new SDKError("deadline", "Call deadline exceeded");
const internal = () => new SDKError("internal", "Internal extension error");

function publicError(error) {
  if (!(error instanceof SDKError)) error = internal();
  const result = { code: error.code, message: error.message };
  if (error.outcome !== undefined) result.outcome = error.outcome;
  return result;
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  promise.catch(() => {});
  return { promise, resolve, reject };
}

function validateScope(scope) {
  if (!object(scope)) throw invalid();
  for (const [key, value] of Object.entries(scope)) {
    if (["generation", "attempt"].includes(key)) {
      if (!uint64(value)) throw invalid();
    } else if (!["plugin", "provider", "account", "session", "operation"].includes(key)
      || typeof value !== "string" || Buffer.byteLength(value) > 4096 || /[\p{Cc}]/u.test(value)) throw invalid();
  }
}

function validateFrame(frame) {
  if (!object(frame) || !identifier(frame.id) || !["call", "cancel", "ack", "host.result"].includes(frame.kind)) throw invalid();
  if (Object.keys(frame).some(key => !["kind", "id", "method", "params", "auth", "result", "error", "seq", "event", "data", "scope", "deadline", "deadlineUnixMs", "parent"].includes(key))) throw invalid();
  if (frame.kind === "call") {
    if (!identifier(frame.method) || ["parent", "seq", "event", "result", "error", "data"].some(key => Object.hasOwn(frame, key))) throw invalid();
    if (frame.scope !== undefined) validateScope(frame.scope);
    for (const key of ["deadline", "deadlineUnixMs"]) {
      if (frame[key] !== undefined && (!Number.isSafeInteger(frame[key]) || frame[key] < 0)) throw invalid();
    }
    if (frame.deadline !== undefined && frame.deadlineUnixMs !== undefined && frame.deadline !== frame.deadlineUnixMs) throw invalid();
  } else if (frame.scope !== undefined && (!object(frame.scope) || Object.keys(frame.scope).length)) {
    throw invalid();
  }
  if (frame.kind === "ack" && (!Number.isSafeInteger(frame.seq) || frame.seq < 1
    || Object.keys(frame).some(key => !["kind", "id", "seq", "scope"].includes(key)))) throw invalid();
  if (frame.kind === "cancel" && Object.keys(frame).some(key => !["kind", "id", "scope"].includes(key))) throw invalid();
  if (frame.kind === "host.result" && (Object.keys(frame).some(key => !["kind", "id", "parent", "result", "error", "scope"].includes(key))
    || frame.error !== undefined && frame.result !== undefined)) throw invalid();
  if (frame.kind === "host.result" && frame.error !== undefined
    && (!object(frame.error) || typeof frame.error.code !== "string" || typeof frame.error.message !== "string"
      || frame.error.outcome !== undefined && typeof frame.error.outcome !== "string")) throw invalid();
}

function checkEvent(event, data) {
  if (!identifier(event)) throw new SDKError("invalid", "Invalid stream event");
  if (event === "data") {
    decodeBytes(data, MAX_CHUNK_BYTES);
  } else {
    if (Buffer.byteLength(stringifyJSON(data)) > MAX_CHUNK_BYTES) throw new SDKError("resource_exhausted", "Stream event exceeds the size limit");
    if (object(data) && Object.hasOwn(data, "body")) decodeBytes(data.body, MAX_CHUNK_BYTES);
    if (event === "accepted" && !object(data)) throw new SDKError("invalid_params", "Acceptance event requires an object");
  }
}

function jsonFieldBytes(text, field) {
  let index = 0, bytes;
  const whitespace = () => { while (text.charCodeAt(index) <= 32) index++; };
  const stringEnd = start => {
    let end = start + 1;
    while (end < text.length) {
      if (text[end] === "\\") end += 2;
      else if (text[end++] === '"') break;
    }
    return end;
  };
  const valueEnd = start => {
    if (text[start] === '"') return stringEnd(start);
    let end = start, depth = 0;
    if (text[start] === "{" || text[start] === "[") {
      do {
        if (text[end] === '"') { end = stringEnd(end); continue; }
        if (text[end] === "{" || text[end] === "[") depth++;
        else if (text[end] === "}" || text[end] === "]") depth--;
        end++;
      } while (depth > 0 && end < text.length);
    } else {
      while (end < text.length && text.charCodeAt(end) > 32 && text[end] !== "," && text[end] !== "}") end++;
    }
    return end;
  };
  whitespace();
  index++;
  while (index < text.length) {
    whitespace();
    if (text[index] === "}") break;
    const keyStart = index;
    index = stringEnd(index);
    const key = JSON.parse(text.slice(keyStart, index));
    whitespace();
    index++;
    whitespace();
    const start = index;
    index = valueEnd(index);
    if (key === field) bytes = Buffer.byteLength(text.slice(start, index));
    whitespace();
    if (text[index++] !== ",") break;
  }
  return bytes;
}

class Writer {
  constructor(output, fail) {
    this.output = output;
    this.queue = [];
    this.active = null;
    this.closed = false;
    this.failed = null;
    this.onError = () => {
      this.stop(new SDKError("transport", "RPC output failed"));
      fail(new SDKError("transport", "RPC output failed"));
    };
    output.on?.("error", this.onError);
  }

  write(frame, signal, started) {
    if (this.closed) return Promise.reject(this.failed || canceled());
    if (signal?.aborted) return Promise.reject(signal.reason);
    let text;
    try {
      text = stringifyJSON(frame);
      if (Buffer.byteLength(text) > MAX_FRAME_BYTES) throw new SDKError("invalid", "RPC frame exceeds the size limit");
    } catch (error) {
      return Promise.reject(error instanceof SDKError ? error : internal());
    }
    const wait = deferred();
    const item = { text: text + "\n", wait, started };
    const abort = () => {
      const index = this.queue.indexOf(item);
      if (index !== -1) this.queue.splice(index, 1);
      wait.reject(signal.reason);
    };
    signal?.addEventListener("abort", abort, { once: true });
    wait.promise.then(() => signal?.removeEventListener("abort", abort), () => signal?.removeEventListener("abort", abort));
    this.queue.push(item);
    this.pump();
    return wait.promise;
  }

  pump() {
    if (this.active || this.closed || !this.queue.length) return;
    const item = this.active = this.queue.shift();
    try {
      item.started?.();
      this.output.write(item.text, error => {
        if (this.active !== item) return;
        this.active = null;
        if (error) {
          item.wait.reject(new SDKError("transport", "RPC output failed"));
          this.onError();
        } else {
          item.wait.resolve();
          this.pump();
        }
      });
    } catch {
      item.wait.reject(new SDKError("transport", "RPC output failed"));
      this.active = null;
      this.onError();
    }
  }

  stop(error = canceled()) {
    if (this.closed) return;
    this.closed = true;
    this.failed = error;
    for (const item of this.queue.splice(0)) item.wait.reject(error);
    this.active?.wait.reject(error);
  }

  dispose() {
    const writing = this.active !== null;
    this.stop();
    if (writing) this.output.destroy?.();
    this.output.off?.("error", this.onError);
  }
}

export async function serve(module, { input = process.stdin, output = process.stdout, shutdownTimeoutMs = 1000 } = {}) {
  const { manifest, handlers, start } = validateModule(module);
  if (!input?.[Symbol.asyncIterator] || typeof output?.write !== "function"
    || !Number.isSafeInteger(shutdownTimeoutMs) || shutdownTimeoutMs < 0 || shutdownTimeoutMs > 30000) {
    throw new SDKError("invalid", "Invalid worker streams or shutdown timeout");
  }
  const description = publicManifest(manifest), calls = new Map(), hostCalls = new Map(), running = new Set();
  const closedCredits = new Map(), closedHostCalls = new Map();
  const transport = deferred(), iterator = input[Symbol.asyncIterator]();
  const writer = new Writer(output, error => transport.reject(error));
  const lifetime = new ComponentLifecycle(manifest, start);
  const lifecycleMethods = new Set(manifest.contract.version.minor >= 1
    ? ["component.configure", "component.ready", "component.stop"] : []);
  let childID = 0, ending = false, failure;

  const remember = (history, id, value) => {
    history.delete(id);
    history.set(id, value);
    if (history.size > 1024) history.delete(history.keys().next().value);
  };

  const stopCall = (record, reason) => {
    record.closed = true;
    if (record.timer !== undefined) clearTimeout(record.timer);
    if (!record.controller.signal.aborted) record.controller.abort(reason);
    record.aborted.reject(reason);
    if (record.event) {
      if (record.event.sent) remember(closedCredits, record.call.id, record.event.seq);
      record.event.wait.reject(reason);
    }
    record.event = null;
    for (const [id, request] of hostCalls) {
      if (request.parent === record) {
        hostCalls.delete(id);
        if (request.sent) remember(closedHostCalls, id, record.call.id);
        request.wait.reject(reason);
      }
    }
  };

  const setDeadline = record => {
    if (!record.deadline || record.closed) return;
    const left = record.deadline - Date.now();
    if (left <= 0) return stopCall(record, deadline());
    record.timer = setTimeout(() => setDeadline(record), Math.min(left, 2147483647));
  };

  const dispatch = async (frame, hostResultBytes) => {
    validateFrame(frame);
    if (frame.kind === "cancel") {
      const record = calls.get(frame.id);
      if (record) stopCall(record, canceled());
      return;
    }
    if (frame.kind === "ack") {
      const record = calls.get(frame.id), event = record?.event;
      if (!record || record.closed) {
        if (closedCredits.get(frame.id) !== frame.seq) throw invalid();
        closedCredits.delete(frame.id);
        return;
      }
      if (!event || !event.sent || event.seq !== frame.seq) throw invalid();
      record.event = null;
      event.wait.resolve();
      return;
    }
    if (frame.kind === "host.result") {
      const request = hostCalls.get(frame.id);
      if (!request) {
        const parent = closedHostCalls.get(frame.id);
        if (parent === undefined || frame.parent !== parent) throw invalid();
        closedHostCalls.delete(frame.id);
        return;
      }
      if (!request.sent) throw invalid();
      if (manifest.contract.version.minor >= 1 ? frame.parent !== request.parent.call.id
        : frame.parent !== undefined && frame.parent !== request.parent.call.id) throw invalid();
      hostCalls.delete(frame.id);
      if (frame.error) {
        try {
          request.wait.reject(new SDKError(frame.error.code, frame.error.message, frame.error.outcome));
        } catch {
          request.wait.reject(internal());
        }
      } else if ((hostResultBytes ?? Buffer.byteLength(stringifyJSON(frame.result ?? null))) > MAX_HOST_BYTES) {
        request.wait.reject(new SDKError("resource_exhausted", "Host result exceeds the size limit"));
      } else request.wait.resolve(frame.result ?? null);
      return;
    }
    if (calls.has(frame.id)) throw new SDKError("invalid", "Duplicate active call ID");
    if (closedCredits.has(frame.id)) throw new SDKError("invalid", "Call ID still has closed stream credit");
    if (calls.size >= MAX_CONCURRENT) {
      await writer.write({ kind: "result", id: frame.id, error: { code: "resource_exhausted", message: "Too many concurrent calls" } });
      return;
    }
    const method = frame.method === "sdk.describe" ? () => description
      : lifecycleMethods.has(frame.method) ? async (ctx, call) => {
        if (call.method === "component.configure") return lifetime.configure(call.params, ctx.signal);
        if (call.params !== null && (!object(call.params) || Object.keys(call.params).length)) throw new SDKError("invalid_params", "Invalid component control parameters");
        if (call.method === "component.stop") { await lifetime.stop(); return {}; }
        return lifetime.ready();
      } : handlers.get(frame.method);
    if (!method) {
      await writer.write({ kind: "result", id: frame.id, error: { code: "method_not_found", message: "Method not supported" } });
      return;
    }
    if (frame.scope?.plugin && frame.scope.plugin !== manifest.id) {
      await writer.write({ kind: "result", id: frame.id, error: { code: "invalid_params", message: "Call belongs to another plugin" } });
      return;
    }
    const call = freezeJSON({
      id: frame.id, method: frame.method, params: frame.params ?? null, scope: frame.scope ?? {},
      ...(frame.auth !== undefined ? { auth: frame.auth } : {}),
      ...((frame.deadlineUnixMs ?? frame.deadline) ? { deadlineUnixMs: frame.deadlineUnixMs ?? frame.deadline } : {}),
    });
    const controller = new AbortController();
    const record = {
      call, controller, aborted: deferred(), deadline: call.deadlineUnixMs, closed: false, event: null, seq: 0,
      hostTail: Promise.resolve(), hostPending: 0,
      actionEffect: frame.method === "module.call" ? manifest.actions?.find(action => action.id === frame.params?.action)?.effect : undefined,
    };
    calls.set(call.id, record);
    setDeadline(record);
    const assertOpen = () => {
      if (record.closed || ending) throw controller.signal.reason instanceof SDKError ? controller.signal.reason : canceled();
    };
    const emit = (event, data = null) => {
      const operation = (async () => {
        assertOpen();
        if (call.method === "module.call" && (record.actionEffect !== "write" || event !== "accepted")) {
          record.creditError = new SDKError("invalid_params", "module.call is unary and permits only a write acceptance receipt");
          throw record.creditError;
        }
        checkEvent(event, data);
        if (record.event) throw new SDKError("backpressure", "Only one stream event may await acknowledgement");
        const wait = deferred(), seq = ++record.seq;
        const credit = record.event = { wait, seq, sent: false };
        try {
          await writer.write({ kind: "event", id: call.id, seq, event, data }, controller.signal, () => { credit.sent = true; });
          await wait.promise;
        } catch (error) {
          if (record.event?.wait === wait) record.event = null;
          throw error;
        }
      })();
      operation.catch(() => {});
      return operation;
    };
    const hostCall = (method, params = null) => {
      if (record.hostPending >= MAX_CONCURRENT) {
        const rejected = Promise.reject(new SDKError("resource_exhausted", "Too many queued host calls"));
        rejected.catch(() => {});
        return rejected;
      }
      const previous = record.hostTail;
      let release;
      record.hostTail = new Promise(resolve => { release = resolve; });
      record.hostPending++;
      const operation = (async () => {
        try {
          await Promise.race([previous, record.aborted.promise]);
          assertOpen();
          if (!["state.get", "state.cas", "network.proxy", "account.replace", "component.call"].includes(method)) throw new SDKError("method_not_found", "Host method not supported");
          const serializedParams = stringifyJSON(params);
          if (Buffer.byteLength(serializedParams) > MAX_HOST_BYTES) throw new SDKError("resource_exhausted", "Host parameters exceed the size limit");
          const payload = parseJSON(serializedParams);
          if (hostCalls.size >= MAX_CONCURRENT) throw new SDKError("resource_exhausted", "Too many pending host calls");
          let id;
          do { id = `sdk.host.${++childID}`; } while (calls.has(id) || hostCalls.has(id) || closedHostCalls.has(id));
          const wait = deferred();
          const request = { parent: record, wait, sent: false };
          hostCalls.set(id, request);
          try {
            await writer.write({
              kind: "host.call", id, parent: call.id, method, params: payload, scope: call.scope,
              ...(record.deadline ? { deadline: record.deadline } : {}),
            }, controller.signal, () => { request.sent = true; });
            return await wait.promise;
          } finally {
            hostCalls.delete(id);
          }
        } finally {
          record.hostPending--;
          release();
        }
      })();
      operation.catch(() => {});
      return operation;
    };
    let stopLifetime;
    const handler = Promise.resolve().then(() => {
      assertOpen();
      let moduleContext;
      if (manifest.contract.version.minor >= 1 && call.method !== "sdk.describe" && !lifecycleMethods.has(call.method)) {
        const bound = lifetime.callContext(controller, reason => stopCall(record, reason));
        moduleContext = bound.module;
        stopLifetime = bound.stop;
        controller.signal.throwIfAborted();
      }
      return method(Object.freeze({ signal: controller.signal, ...(moduleContext ? { module: moduleContext } : {}) }), call, Object.freeze({ emit, hostCall }));
    }).finally(() => stopLifetime?.());
    handler.catch(() => {});
    const task = (async () => {
      try {
        const result = await Promise.race([handler, record.aborted.promise]);
        if (record.creditError) throw record.creditError;
        if (record.event) await record.event.wait.promise;
        if (record.hostPending) await Promise.race([record.hostTail, record.aborted.promise]);
        assertOpen();
        stopCall(record, canceled());
        try {
          await writer.write({ kind: "result", id: call.id, result: result ?? null });
        } catch (error) {
          if (error.code === "transport" || writer.closed) throw error;
          await writer.write({ kind: "result", id: call.id, error: publicError(error) });
        }
      } catch (error) {
        stopCall(record, error instanceof SDKError ? error : internal());
        if (!writer.closed) await writer.write({ kind: "result", id: call.id, error: publicError(error) });
      } finally {
        stopCall(record, canceled());
        await handler.catch(() => {});
        calls.delete(call.id);
      }
    })();
    running.add(task);
    task.catch(error => transport.reject(error instanceof SDKError ? error : internal()))
      .finally(() => running.delete(task));
  };

  let pieces = [], length = 0;
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const line = async () => {
    let frame, text;
    try { text = decoder.decode(Buffer.concat(pieces, length)); frame = parseJSON(text); } catch { throw invalid(); }
    pieces = [];
    length = 0;
    await dispatch(frame, frame?.kind === "host.result" ? jsonFieldBytes(text, "result") : undefined);
  };
  try {
    while (true) {
      const item = await Promise.race([iterator.next(), transport.promise]);
      if (item.done) break;
      if (typeof item.value !== "string" && !(item.value instanceof Uint8Array)) throw invalid();
      const bytes = typeof item.value === "string" ? Buffer.from(item.value) : Buffer.from(item.value.buffer, item.value.byteOffset, item.value.byteLength);
      let start = 0;
      for (let index = bytes.indexOf(10); ; index = bytes.indexOf(10, start)) {
        const end = index === -1 ? bytes.length : index;
        const part = bytes.subarray(start, end);
        length += part.length;
        if (length > MAX_FRAME_BYTES) throw new SDKError("invalid", "RPC frame exceeds the size limit");
        if (part.length) pieces.push(Buffer.from(part));
        if (index === -1) break;
        await line();
        start = index + 1;
      }
    }
    if (length) throw new SDKError("invalid", "Incomplete RPC frame at end of input");
  } catch (error) {
    failure = error instanceof SDKError ? error : new SDKError("transport", "RPC input failed");
  } finally {
    ending = true;
    const stopped = lifetime.stop();
    for (const record of calls.values()) stopCall(record, canceled());
    let timer;
    await Promise.race([
      Promise.allSettled([...running, stopped]),
      new Promise(resolve => { timer = setTimeout(resolve, shutdownTimeoutMs); }),
    ]);
    clearTimeout(timer);
    writer.dispose();
    if (failure) input.destroy?.();
    try { iterator.return?.()?.catch?.(() => {}); } catch {}
  }
  if (failure) throw failure;
}
