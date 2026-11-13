// Parent-side page calls, shared by legacy windows and SDK-installed modules.
// The frame supplies payloads; its parent supplies the immutable owner ref.
export async function answerPluginPage(p, m, theme, titled) {
  const e = p.entry;
  const signal = p.abort?.signal;
  const request = (path, body) => api(path, body, undefined, signal);
  const sdk = () => {
    if (!e.ref) throw new Error(t("No such call: {what}", { what: String(m.method) }));
  };
  switch (m.method) {
    case "hello":
      p.hello = true;
      clearTimeout(p.timer);
      p.said?.();
      return { ...theme(), context: p.context || {}, service: ((e.ref || !p.context) && e.service) || "", visible: p.shown ?? true };
    case "theme":
      return theme();
    case "backend.call":
      sdk();
      p.instance ||= Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
      return request("plugin-pages/call", { ref: e.ref, request: p.instance + ":" + m.id, binding: m.params?.binding, input: m.params?.input ?? null });
    case "config.get":
      sdk();
      return request("plugin-pages/config", { ref: e.ref });
    case "config.cas": {
      sdk();
      if (typeof m.params?.expected !== "string" || m.params?.values === undefined) throw new Error(t("No such call: {what}", { what: "config.cas" }));
      const result = await request("plugin-pages/config", { ref: e.ref, expected: m.params.expected, values: m.params.values });
      // The response belongs to this frame; the next catalog notification
      // replaces it with a frame bound to the new configuration generation.
      return result;
    }
    case "options.get":
    case "options.set": {
      if (e.ref) throw new Error(t("No such call: {what}", { what: String(m.method) }));
      if (m.method === "options.get") return request("plugin-pages/options", { spec: e.spec });
      const o = m.params?.options;
      if (!o || typeof o !== "object" || Array.isArray(o)) throw new Error(t("A page's options are an object"));
      return request("plugin-pages/options", { spec: e.spec, options: o });
    }
    case "read": {
      const name = String(m.params?.name || "");
      if (!e.reads.includes(name)) throw new Error(t("{name} doesn't ask to read {what}", { name: titled(e), what: name }));
      return request("plugin-pages/read", { ...(e.ref ? { ref: e.ref } : { spec: e.spec }), read: name, params: m.params?.params || {} });
    }
    case "asset":
      return import("./pageshare.js").then(() => pageAsset(e.url, m.params?.url, signal));
    case "image.copy":
    case "image.save":
      return import("./pageshare.js").then(() => pageShare(e.spec, m.method === "image.save", m.params));
  }
  throw new Error(t("No such call: {what}", { what: String(m.method) }));
}

// Catalog/config notifications do not start a worker or poll providers. A
// failed notification read preserves the current UI and retries with a delay.
export function watchExtensionWindows(load) {
  const abort = new AbortController();
  let stopped = false, version, retry;
  const next = async () => {
    if (stopped) return;
    try {
      const result = await api("plugin-pages/changes" + (version === undefined ? "" : "?version=" + encodeURIComponent(version)), undefined, undefined, abort.signal);
      if (typeof result?.version !== "string") return;
      if ((version === undefined || version !== result.version) && await load() === false) throw new Error("Window catalog unavailable");
      version = result.version;
      if (!stopped) retry = setTimeout(next, 0);
    } catch {
      if (!stopped) retry = setTimeout(next, 3000);
    }
  };
  next();
  const stop = () => { stopped = true; abort.abort(); clearTimeout(retry); };
  addEventListener("pagehide", stop, { once: true });
  return stop;
}
