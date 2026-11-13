import { Buffer } from "node:buffer";
import { MAX_HOST_BYTES, SDKError } from "./bytes.js";
import { componentID, object, validID, validReadyURL, validServiceContract } from "./composite.js";
import { freezeJSON, jsonSnapshot, stringifyJSON } from "./manifest.js";

const canceled = () => new SDKError("canceled", "Component stopped");
const invalid = message => new SDKError("invalid_params", message);
const identity = value => typeof value === "string" && value !== "" && Buffer.byteLength(value) <= 256 && !/[\s\p{Cc}]/u.test(value);
const only = (value, fields) => object(value) && Object.keys(value).every(key => fields.includes(key));
const version = value => typeof value === "string" && value.length <= 20 && /^(0|[1-9][0-9]*)$/.test(value) && BigInt(value) <= 18446744073709551615n;
const contract = value => only(value ?? {}, ["id", "version"]) && (value?.id === undefined || typeof value.id === "string")
  && (value?.version === undefined || Number.isInteger(value.version) && value.version >= 0 && value.version <= 4294967295);
const sameContract = (left = {}, right = {}) => (left.id ?? "") === (right.id ?? "") && (left.version ?? 0) === (right.version ?? 0);

function endpoints(value) {
  if (value == null) return {};
  if (!object(value) || Object.keys(value).length > 128
    || Object.entries(value).some(([id, url]) => !validID(id) || !validReadyURL(url))) throw invalid("Invalid component readiness endpoint");
  return jsonSnapshot(value);
}

function configuration(params, manifest) {
  if (!only(params, ["component", "contract", "instance", "config", "services"])
    || params.component !== componentID(manifest) || !contract(params.contract)
    || !sameContract(params.contract, manifest.component?.contract)
    || !identity(params.instance) || !only(params.config, ["version", "values"])
    || !version(params.config.version) || !Object.hasOwn(params.config, "values")
    || Buffer.byteLength(stringifyJSON(params.config.values)) > MAX_HOST_BYTES) throw invalid("Component identity or configuration snapshot is invalid");
  const services = params.services ?? {};
  if (!object(services) || Object.keys(services).length > 1024) throw invalid("Invalid component dependencies");
  for (const [id, ready] of Object.entries(services)) {
    if (!validID(id) || !only(ready, ["component", "contract", "instance", "configVersion", "endpoints"])
      || ready.component !== id || !contract(ready.contract) || !validServiceContract(ready.contract)
      || !identity(ready.instance) || ready.configVersion !== params.config.version) throw invalid("Invalid configured component dependency");
    endpoints(ready.endpoints);
  }
  return jsonSnapshot({
    component: params.component, contract: { id: params.contract?.id ?? "", version: params.contract?.version ?? 0 },
    instance: params.instance, config: params.config,
    services: Object.fromEntries(Object.entries(services).map(([id, ready]) => [id, { ...ready, endpoints: ready.endpoints ?? {} }])),
  });
}

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (object(value)) return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])]));
  return value;
}
function awaitSignal(promise, signal) {
  if (signal.aborted) return Promise.reject(signal.reason);
  let abort;
  const stopped = new Promise((_, reject) => { abort = () => reject(signal.reason); signal.addEventListener("abort", abort, { once: true }); });
  return Promise.race([promise, stopped]).finally(() => signal.removeEventListener("abort", abort));
}
async function disposeSafely(dispose) {
  try { await dispose?.(); } catch { /* disposal never publishes private diagnostics */ }
}

export class ComponentLifecycle {
  constructor(manifest, start) {
    this.manifest = manifest;
    this.start = start;
    this.controller = new AbortController();
    this.closed = false;
  }

  async configure(params, parentSignal) {
    const request = configuration(params, this.manifest);
    if (this.closed) throw canceled();
    if (this.request) {
      if (this.request.component !== request.component || this.request.instance !== request.instance
        || !sameContract(this.request.contract, request.contract)
        || this.request.config.version !== request.config.version
        || stringifyJSON(this.request.config.values) !== stringifyJSON(request.config.values)
        || stringifyJSON(canonical(this.request.services)) !== stringifyJSON(canonical(request.services))) {
        throw new SDKError("conflict", "Component configuration is immutable for this instance");
      }
    } else {
      this.request = request;
      const abort = () => this.controller.abort(parentSignal.reason ?? canceled());
      parentSignal.addEventListener("abort", abort, { once: true });
      if (parentSignal.aborted) abort();
      this.initializing = this.initialize(request, parentSignal).finally(() => parentSignal.removeEventListener("abort", abort));
      this.initializing.catch(() => {});
    }
    await awaitSignal(this.initializing, parentSignal);
    return this.ready();
  }

  async initialize(request, parentSignal) {
    let dispose;
    try {
      const value = await this.start?.(Object.freeze({ signal: this.controller.signal }), this.snapshot()) ?? {};
      if (!object(value) || value.dispose !== undefined && typeof value.dispose !== "function") throw invalid("Invalid component startup result");
      dispose = value.dispose;
      if (this.controller.signal.aborted) throw this.controller.signal.reason ?? canceled();
      if (parentSignal.aborted) throw parentSignal.reason ?? canceled();
      const readyEndpoints = endpoints(value.endpoints);
      if (this.closed) throw canceled();
      this.result = {
        component: request.component, contract: request.contract, instance: request.instance,
        configVersion: request.config.version, endpoints: readyEndpoints,
      };
      this.state = value.state;
      this.dispose = dispose;
      dispose = undefined;
    } catch (error) {
      this.controller.abort(error instanceof SDKError ? error : new SDKError("internal", "Component startup failed"));
      this.error = error instanceof SDKError ? error : new SDKError("internal", "Component startup failed");
      await disposeSafely(dispose);
      throw this.error;
    }
  }

  ready() {
    if (this.closed) throw canceled();
    if (!this.request) throw new SDKError("not_ready", "Component has not been configured");
    if (this.error) throw this.error;
    if (!this.result) throw new SDKError("not_ready", "Component startup has not completed");
    return jsonSnapshot(this.result);
  }

  snapshot() {
    if (!this.request) return undefined;
    const result = jsonSnapshot({
      component: this.request.component, instance: this.request.instance,
      config: this.request.config, services: this.request.services,
    });
    if (this.state !== undefined) result.state = this.state;
    return result;
  }

  callContext(controller, onAbort) {
    this.ready();
    const abort = () => onAbort(this.controller.signal.reason ?? canceled());
    this.controller.signal.addEventListener("abort", abort, { once: true });
    if (this.controller.signal.aborted) abort();
    const snapshot = this.snapshot();
    const state = snapshot.state;
    delete snapshot.state;
    freezeJSON(snapshot);
    return {
      module: Object.freeze({ ...snapshot, ...(state !== undefined ? { state } : {}) }),
      stop: () => this.controller.signal.removeEventListener("abort", abort),
    };
  }

  stop() {
    if (!this.stopping) {
      this.closed = true;
      this.controller.abort(canceled());
      this.stopping = (async () => {
        await this.initializing?.catch(() => {});
        const dispose = this.dispose;
        this.dispose = undefined;
        await disposeSafely(dispose);
      })();
    }
    return this.stopping;
  }
}

export function moduleContextFrom(ctx) {
  if (ctx?.module === undefined) return undefined;
  const { state, ...value } = ctx.module;
  return { ...jsonSnapshot(value), ...(state !== undefined ? { state } : {}) };
}

export function moduleHTTPHeaders(context) {
  return {
    "X-Magpie-Component-Instance": context.instance,
    "X-Magpie-Component": context.component,
    "X-Magpie-Component-Config": context.config.version,
  };
}
