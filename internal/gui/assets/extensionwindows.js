// Parent-side page calls, shared by legacy windows and SDK-installed modules.
// The frame supplies payloads; its parent supplies the immutable owner ref.
const THEME_TOKENS = ["bg", "card", "fg", "fg-2", "muted", "faint", "line", "accent", "accent-soft", "accent-fg", "green", "red", "amber", "font", "code"];

export function pluginTheme() {
  const root = document.documentElement;
  const css = getComputedStyle(root);
  const tokens = {};
  for (const key of THEME_TOKENS) tokens[key] = css.getPropertyValue("--" + key).trim();
  const dark = root.dataset.theme === "dark"
    || (root.dataset.theme !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
  return { dark, lang: locale, tokens };
}

export async function answerPluginPage(p, m, theme, titled) {
  const e = p.entry;
  const signal = p.abort?.signal;
  const request = (path, body) => api(path, body, undefined, signal);
  const visible = () => p.checkVisibility ? p.checkVisibility() : p.shown === true;
  const sdk = () => {
    if (!e.ref) throw new Error(t("No such call: {what}", { what: String(m.method) }));
  };
  switch (m.method) {
    case "hello":
      p.hello = true;
      clearTimeout(p.timer);
      p.said?.();
      {
        const result = { ...theme(), context: p.context || {}, service: ((e.ref || !p.context) && e.service) || "" };
        p.history = false;
        if (e.ref && m.params?.features?.includes("session-history-v1")) {
          const negotiated = await request("plugin-pages/history", { ref: e.ref, operation: "hello" });
          p.history = negotiated?.features?.includes("session-history-v1") === true;
          result.features = p.history ? ["session-history-v1"] : [];
        }
        return { ...result, visible: visible() };
      }
    case "theme":
      return theme();
    case "backend.call":
      sdk();
      p.instance ||= Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
      return request("plugin-pages/call", { ref: e.ref, request: p.instance + ":" + m.id, binding: m.params?.binding, input: m.params?.input ?? null, ...(m.params?.options?.account === undefined ? {} : { account: m.params.options.account }) });
    case "backend.accounts":
      sdk();
      return request("plugin-pages/accounts", { ref: e.ref, binding: m.params?.binding });
    case "auth.start":
    case "auth.finish":
    case "auth.cancel":
      sdk();
      p.instance ||= Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
      return request("plugin-pages/auth", { ref: e.ref, binding: m.params?.binding, parent: p.instance, request: p.instance + ":" + m.id, operation: m.method.slice(5), ...(m.method === "auth.start" ? { options: m.params?.options } : { flow: m.params?.flow }) });
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
    case "history.inspect":
    case "history.request":
    case "history.release":
    case "history.list":
    case "history.read":
    case "history.media": {
      sdk();
      if (!p.history || !visible() || signal?.aborted) throw new Error("Session history is unavailable");
      const visibilityVersion = p.visibilityVersion;
      const current = () => {
        if (!visible() || signal?.aborted || p.entry !== e || p.visibilityVersion !== visibilityVersion) throw new Error("History window changed");
      };
      const operation = m.method.slice(8);
      const keys = {
        inspect: ["source"], request: ["source", "viewRef", "rootRef"],
        release: ["source", "viewRef", "consentRef"],
        list: ["source", "viewRef", "rootRef", "cursor", "limit"],
        read: ["source", "viewRef", "sessionRef"],
        media: ["source", "viewRef", "sessionRef", "readRef", "mediaRef"],
      }[operation];
      const params = m.params;
      if (!params || typeof params !== "object" || Array.isArray(params) ||
          Object.keys(params).some(key => !keys.includes(key))) throw new Error("Invalid history selection");
      const body = { ref: e.ref, operation };
      for (const key of keys) if (params[key] !== undefined) body[key] = params[key];
      if (operation === "request") {
        const inspection = await request("plugin-pages/history", { ref: e.ref, operation: "inspect", source: body.source });
        current();
        const root = inspection.roots.find(root => root.rootRef === body.rootRef && root.available);
        if (!root || inspection.viewRef !== body.viewRef || inspection.ownOff) throw new Error("History selection changed");
        const message = `${inspection.module} / ${inspection.source}\n${inspection.agent} / ${root.slot}\n` +
          t("Delegate this selected history root to this package and configuration.") + "\n" +
          (inspection.readAllowed ? "" : t("Also allow Magpie to read this native agent's sessions.") + "\n") +
          (inspection.replaces.length ? t("Replace current delegation: {delegations}.", { delegations: inspection.replaces.join(", ") }) : "");
        const accepted = await confirmAction(t("Sessions"), message, t("Continue"));
        current();
        if (!accepted) {
          const inspection = await request("plugin-pages/history", { ref: e.ref, operation: "inspect", source: body.source });
          current();
          return { state: "canceled", inspection };
        }
      }
      if (operation === "media") {
        const response = await fetch("/api/plugin-pages/history", {
          method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body), signal,
        });
        if (!response.ok || Number(response.headers.get("Content-Length")) > (8 << 20)) throw new Error("History media is unavailable");
        const reader = response.body.getReader();
        const chunks = [];
        let size = 0;
        try {
          current();
          for (;;) {
            const { done, value } = await reader.read();
            current();
            if (done) break;
            size += value.byteLength;
            if (size > (8 << 20)) throw new Error("History media exceeds its byte budget");
            chunks.push(value);
          }
        } catch (error) {
          await reader.cancel();
          throw error;
        }
        current();
        const bytes = new Uint8Array(size);
        let offset = 0;
        for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
        return { mime: response.headers.get("Content-Type") || "application/octet-stream", data: bytes.buffer };
      }
      const result = await request("plugin-pages/history", body);
      current();
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

// The renderer has one last await before posting the result to the frame.
export function checkPluginReply(p, method, visibilityVersion, result) {
  const visible = p.checkVisibility();
  if (method.startsWith("history.") && (!visible || p.visibilityVersion !== visibilityVersion)) throw new Error("History window changed");
  if (method === "hello") result.visible = visible;
}

// Ancestor visibility belongs to the renderer's page and panel records.
export function watchPluginFrames(pages, panels, refresh, dispose, post) {
  const all = () => [...pages.values(), ...panels.values()];
  const pushTheme = () => {
    const data = pluginTheme();
    for (const p of all()) if (p.hello) post(p.frame?.contentWindow, { event: "theme", data });
  };
  const themeObserver = new MutationObserver(pushTheme);
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "lang", "class", "style"] });
  const colorScheme = matchMedia("(prefers-color-scheme: dark)");
  colorScheme.addEventListener?.("change", pushTheme);
  const resize = () => { for (const p of all()) refresh(p); };
  const observer = new MutationObserver(changes => {
    for (const [f, p] of panels) if (!f.isConnected) { panels.delete(f); dispose(p); }
    for (const p of all()) {
      if (p.frame && changes.some(change => change.target.contains(p.frame))) refresh(p);
    }
  });
  observer.observe(document.documentElement, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden", "class", "style"] });
  addEventListener("resize", resize);
  resize();
  return () => {
    themeObserver.disconnect();
    colorScheme.removeEventListener?.("change", pushTheme);
    observer.disconnect();
    removeEventListener("resize", resize);
  };
}

// Catalog/config notifications do not start a worker or poll providers. A
// failed notification read preserves the current UI and retries with a delay.
export function watchExtensionWindows(load) {
  let active, version, retry;
  const next = async (poll) => {
    if (active !== poll) return;
    try {
      const result = await api("plugin-pages/changes" + (version === undefined ? "" : "?version=" + encodeURIComponent(version)), undefined, undefined, poll.signal);
      if (active !== poll) return;
      if (typeof result?.version !== "string") throw new Error("Invalid window catalog version");
      if ((version === undefined || version !== result.version) && await load() === false) throw new Error("Window catalog unavailable");
      if (active !== poll) return;
      version = result.version;
      retry = setTimeout(() => next(poll), 0);
    } catch {
      if (active === poll) retry = setTimeout(() => next(poll), 3000);
    }
  };
  const resume = () => { if (!active) next(active = new AbortController()); };
  const pause = () => { active?.abort(); active = undefined; clearTimeout(retry); };
  const hide = (event) => { if (event.persisted) pause(); else stop(); };
  const stop = () => {
    pause();
    removeEventListener("pagehide", hide);
    removeEventListener("pageshow", resume);
  };
  addEventListener("pagehide", hide);
  addEventListener("pageshow", resume);
  resume();
  return stop;
}
