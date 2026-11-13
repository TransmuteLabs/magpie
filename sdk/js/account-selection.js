import { SDKError } from "./errors.js";

export function validateAccountSelection(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
    Object.keys(value).some(key => !["id", "expectedGeneration"].includes(key)) ||
    typeof value.id !== "string" || !value.id || new TextEncoder().encode(value.id).byteLength > 256 || /[\x00-\x1f\x7f]/u.test(value.id) ||
    typeof value.expectedGeneration !== "string" || !/^[1-9][0-9]{0,19}$/.test(value.expectedGeneration) ||
    BigInt(value.expectedGeneration) > 18446744073709551615n) {
    throw new SDKError("invalid_params", "Account selection requires an identity and canonical nonzero uint64 generation");
  }
  return value;
}

function summary(value) {
  validateAccountSelection({ id: value?.id, expectedGeneration: value?.generation });
  if (value.expiresAt !== undefined && (!Number.isSafeInteger(value.expiresAt) || value.expiresAt < 0)) {
    throw new SDKError("invalid_params", "Invalid account expiry");
  }
  return Object.freeze({ id: value.id, generation: value.generation,
    ...(value.expiresAt === undefined ? {} : { expiresAt: value.expiresAt }) });
}

function jsonCopy(value) {
  const encoded = JSON.stringify(value, (_, item) => {
    if (item === undefined || typeof item === "function" || typeof item === "symbol" ||
      typeof item === "bigint" || typeof item === "number" && !Number.isFinite(item)) {
      throw new SDKError("invalid_params", "Workflow input must be JSON");
    }
    return item;
  });
  return JSON.parse(encoded);
}

function flowView(value) {
  if (!value || typeof value.flow !== "string" || !value.flow ||
    !["waiting", "pending", "complete", "canceled", "expired", "uncertain"].includes(value.state) ||
    value.retryAfterMs !== undefined && (!Number.isSafeInteger(value.retryAfterMs) || value.retryAfterMs < 0)) {
    throw new SDKError("invalid_params", "Invalid authentication flow view");
  }
  for (const key of ["url", "userCode"]) {
    if (value[key] !== undefined && typeof value[key] !== "string") {
      throw new SDKError("invalid_params", "Invalid authentication display field");
    }
  }
  return Object.freeze({ flow: value.flow, state: value.state,
    ...(value.url === undefined ? {} : { url: value.url }),
    ...(value.userCode === undefined ? {} : { userCode: value.userCode }),
    ...(value.retryAfterMs === undefined ? {} : { retryAfterMs: value.retryAfterMs }),
    ...(value.account === undefined ? {} : { account: summary(value.account) }) });
}

const terminal = value => value && !["waiting", "pending"].includes(value.state);

export function createAccountWorkflow(binding, authBinding) {
  if (typeof binding !== "string" || !binding ||
    authBinding !== undefined && (typeof authBinding !== "string" || !authBinding)) {
    throw new SDKError("invalid_params", "Workflow requires declared binding names");
  }
  let accounts = Object.freeze([]), selection = null, stale = false, flow = null;
  let ticket = 0, active = 0, startKey, starting, finishing, canceling;
  const bridge = () => {
    const value = globalThis.window?.magpie;
    if (!value?.backend?.accounts || !value?.backend?.call ||
      authBinding !== undefined && (!value.auth?.start || !value.auth?.finish || !value.auth?.cancel)) {
      throw new Error("Magpie account page bridge is unavailable");
    }
    return value;
  };
  const invalidate = () => { selection = null; stale = true; };
  const selected = () => {
    if (!selection) throw new SDKError("invalid_params", "Select a currently listed account first");
    return { ...selection };
  };
  const run = async operation => {
    active++;
    try { return await operation(); } finally { active--; }
  };
  const refresh = () => {
    const current = ++ticket;
    return run(async () => {
      try {
        const values = await bridge().backend.accounts(binding);
        if (!Array.isArray(values) || values.length > 1024) throw new SDKError("invalid_params", "Invalid account list");
        const next = values.map(summary);
        if (new Set(next.map(value => value.id)).size !== next.length) {
          throw new SDKError("invalid_params", "Duplicate account identities");
        }
        if (current === ticket) {
          accounts = Object.freeze(next);
          if (selection && !accounts.some(value => value.id === selection.id && value.generation === selection.expectedGeneration)) invalidate();
        }
        return Object.freeze(next);
      } catch (error) {
        if (current === ticket) { accounts = Object.freeze([]); invalidate(); }
        throw error;
      }
    });
  };
  const acceptFlow = async value => {
    const next = flowView(value);
    if (flow && flow.flow !== next.flow) throw new SDKError("invalid_params", "Authentication flow changed");
    if (terminal(flow)) return flow;
    flow = next;
    if (next.state === "complete") await refresh();
    return flow;
  };
  const requireAuth = () => {
    if (!authBinding) throw new SDKError("invalid_params", "Workflow has no authentication binding");
  };
  const workflow = {
    get snapshot() {
      return Object.freeze({ accounts, selection: selection ? Object.freeze({ ...selection }) : null,
        stale, flow, busy: active > 0 });
    },
    refresh,
    select(id) {
      selection = null;
      if (id === null) { stale = false; return null; }
      const account = accounts.find(value => value.id === id);
      if (!account) throw new SDKError("invalid_params", "Account is not in the current host list");
      selection = { id: account.id, expectedGeneration: account.generation };
      stale = false;
      return Object.freeze({ ...selection });
    },
    call(input = null) {
      const account = selected();
      return run(async () => {
        try { return await bridge().backend.call(binding, jsonCopy(input), { account }); }
        catch (error) {
          if (selection?.id === account.id && selection.expectedGeneration === account.expectedGeneration) {
            accounts = Object.freeze([]);
            invalidate();
          }
          throw error;
        }
      });
    },
    start(options, reauthorize = false) {
      requireAuth();
      if (!options || typeof options !== "object" || Array.isArray(options) ||
        Object.keys(options).some(key => !["method", "input"].includes(key)) ||
        typeof options.method !== "string" || !options.method || typeof reauthorize !== "boolean") {
        throw new SDKError("invalid_params", "Authentication uses a method and optional JSON input");
      }
      const request = { method: options.method,
        ...(options.input === undefined ? {} : { input: jsonCopy(options.input) }),
        ...(reauthorize ? { account: selected() } : {}) };
      const key = JSON.stringify(request);
      if (starting && !terminal(flow)) {
        if (key !== startKey) throw new SDKError("invalid_params", "An authentication intent is already active");
        return starting;
      }
      if (finishing || canceling) throw new SDKError("invalid_params", "Authentication is settling");
      flow = null;
      startKey = key;
      starting = run(async () => acceptFlow(await bridge().auth.start(authBinding, request)));
      return starting;
    },
    finish() {
      requireAuth();
      if (finishing) return finishing;
      const pending = run(async () => {
        if (starting) await starting;
        if (!flow) throw new SDKError("invalid_params", "Start authentication first");
        if (terminal(flow)) return flow;
        return acceptFlow(await bridge().auth.finish(authBinding, flow.flow));
      });
      finishing = pending;
      pending.finally(() => { if (finishing === pending) finishing = null; }).catch(() => {});
      return pending;
    },
    cancel() {
      requireAuth();
      if (canceling) return canceling;
      const pending = run(async () => {
        if (starting) await starting;
        if (!flow) throw new SDKError("invalid_params", "Start authentication first");
        if (terminal(flow)) return flow;
        return acceptFlow(await bridge().auth.cancel(authBinding, flow.flow));
      });
      canceling = pending;
      pending.finally(() => { if (canceling === pending) canceling = null; }).catch(() => {});
      return pending;
    },
  };
  return Object.freeze(workflow);
}
