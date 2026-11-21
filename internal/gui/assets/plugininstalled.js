// Installed narrowed as Discover is (WJaunita45604 on X): the same strip of
// kinds and the same Origin menu, over what is installed. Each plugin is
// put where Discover puts it: its card's kind and origin (kindOf, srcOf),
// and for one the market doesn't list, the same read from what it is: its
// chips' kind, a git one on GitHub, a @magpie-community package the
// community's, else another author's. plugins.js imports this when its
// page is first drawn, so it weighs nothing on the core; until then the
// list is drawn whole. st holds the picks; the strip and the menu show
// only where there are two or more to pick from.
window.pmInstalled = function ({ es, st, everything, entryOf, kindOf, srcOf, name, isGit, ORDER, SOURCES, kindStrip, originButton, drawBody }) {
  const card = new Map();
  for (const l of everything(() => true)) { const e = entryOf(l.package); if (e && !card.has(e)) card.set(e, l); }
  const sorted = es.map((e) => {
    if (e.sdk) {
      const kinds = [
        e.providers?.length && "pv",
        (e.isMiddleware || e.isGateway) && "mw",
        e.isAgent && "ag",
        e.sdk.capabilities?.includes("tool-provider") && "tl",
        e.sdk.window && "pg",
      ].filter(Boolean);
      return { e, kinds, src: e.sdk.source ? "gh" : "third" };
    }
    const l = card.get(e);
    if (l) return { e, kinds: [kindOf(l)], src: srcOf(l) };
    const kind = e.middleware || e.isMiddleware || e.gateway || e.isGateway ? "mw" : e.agent || e.isAgent ? "ag" : e.tools?.length && !e.providers?.length ? "tl" : e.page && !e.providers?.length ? "pg" : "pv";
    return { e, kinds: [kind], src: isGit(e.spec) ? "gh" : name(e.spec).startsWith("@magpie-community/") ? "ours" : "third" };
  });
  const kinds = ORDER.filter((k) => sorted.some((x) => x.kinds.includes(k)));
  const srcs = SOURCES.map(([v]) => v).filter((v) => v && sorted.some((x) => x.src === v));
  if (!kinds.includes(st.kind) || kinds.length < 2) st.kind = "";
  if (!srcs.includes(st.src) || srcs.length < 2) st.src = "";
  const base = sorted.filter((x) => !st.src || x.src === st.src);
  const shown = base.filter((x) => !st.kind || x.kinds.includes(st.kind)).map((x) => x.e);
  let none = null;
  if (!shown.length) {
    // a pick that leaves nothing says so, with the way back
    none = el("div", "pm-ghnone");
    const every = el("button", "text", t("Show all"));
    every.type = "button";
    every.onclick = () => { st.kind = st.src = ""; drawBody(); };
    none.append(el("span", "", t("No plugin matches")), every);
  }
  return {
    shown, none,
    strip: kinds.length > 1 ? kindStrip(kinds, (k) => base.filter((x) => !k || x.kinds.includes(k)).length, st.kind, (k) => { st.kind = k; drawBody(); }) : null,
    pop: srcs.length > 1 ? originButton(st.src, null, (v) => { st.src = v; drawBody(); }) : null,
  };
};

window.pmSDKSubscriptions = function (entry) {
  if (!providers || providers.last || entry.off || !entry.sdk.capabilities?.includes("account-auth")) return [];
  const grants = sdkSubs();
  return (entry.sdk.providerIDs || []).flatMap((id, index) => {
    const grant = grants.find((sub) => sub.id === id && (sub.page || sub.revision && sub.methods?.length));
    const signedIn = !!hasLogins(id);
    return grant || signedIn ? [{ id, name: entry.providers[index], icon: grant?.icon, signedIn }] : [];
  });
};

window.pmSDKDetails = function ({ sdk, providers }, gitWeb) {
  const box = el("details", "sub pm-sdk");
  box.append(el("summary", "", "SDK " + sdk.api + " · " + sdk.runtime));
  if (sdk.description) box.append(el("div", "", sdk.description));
  const line = (label, items) => {
    if (!items?.length) return;
    const row = el("div");
    row.append(el("b", "", t(label)), document.createTextNode(" · " + items.join(t(", "))));
    box.append(row);
  };
  line("Providers", providers);
  line("Agents", sdk.agents);
  line("Protocols", sdk.protocols);
  line("Gateway", sdk.inbounds);
  line("Sessions", sdk.sessions);
  line("Capabilities", sdk.capabilities);
  line("Compatibility", [sdk.hostAPI && "SDK " + sdk.hostAPI, sdk.nativeABI && "ABI " + sdk.nativeABI, ...(sdk.platforms || [])].filter(Boolean));
  if (sdk.window) line("Interface", [t("Available")]);
  if (sdk.source) {
    const ref = el("a", "pm-link", sdk.source);
    const href = gitWeb(sdk.source.replace(/@[^#]+/, ""));
    ref.href = href;
    ref.onclick = (ev) => { ev.preventDefault(); api("open", { url: href }).catch(statusErr); };
    const row = el("div");
    row.append(el("b", "", t("Source")), document.createTextNode(" · "), ref);
    box.append(row);
  }
  return box;
};

window.pmPluginOptions = function (entry, draft, operations) {
  const settings = entry.sdk?.settings;
  const middleware = entry.middleware;
  const ownOptions = !entry.isMiddleware && !entry.isGateway && !!(entry.options || entry.optionsExample);
  const available = entry.sdk ? !!settings : entry.isGateway || (middleware && (draft || !middleware.error)) || ownOptions;
  if (!available) return;

  const { busy, edit, save, name } = operations;
  const done = () => status(t("{name}'s options saved", { name }), "ok");
  if (draft) {
    const editor = settings ? sdkOptionsForm(draft, busy, (values) => {
      save("settings", values, () => { edit(null); done(); });
    }, () => edit(null)) : jsonOptionsForm(entry, draft, operations, done);
    return { editor };
  }

  const button = el("button", "text", t("Options"));
  button.title = settings || entry.options ? t("The options it runs with") : t("It runs with no options; its package suggests some");
  button.disabled = busy;
  button.onclick = () => {
    edit(settings ? { base: settings, values: Object.create(null) }
      : { text: JSON.stringify(entry.options || entry.optionsExample || {}, null, 2), error: "" });
    document.querySelector("#view-plugins .pm-opts textarea, #view-plugins .pm-sdk-opts :is(input, button)")?.focus(NO_SCROLL);
  };
  return { button };
};

function sdkOptionsForm(draft, busy, save, cancel) {
  const { base, values } = draft;
  const box = el("div", "pm-opts pm-sdk-opts");
  const fields = el("div", "list prefs");
  const bar = el("div", "pm-opts-bar");
  const ok = el("button", "text primary", t("Save"));
  const no = el("button", "text", t("Cancel"));
  ok.disabled = busy || !Object.keys(values).length;
  no.disabled = busy;
  no.onclick = cancel;
  ok.onclick = () => {
    if (fields.querySelector('[aria-invalid="true"]')) return;
    const { packageRevision, configVersion, enableGeneration } = base;
    save({ packageRevision, configVersion, enableGeneration, values });
  };
  box.onkeydown = (ev) => { if (ev.key === "Escape" && !busy) cancel(); };
  fields.append(...magpie.ui.form(base.schema, { ...base.values, ...values }, (change) => {
    const [key, value] = Object.entries(change)[0];
    if (base.values[key] === value) delete values[key];
    else values[key] = value;
    ok.disabled = !Object.keys(values).length;
  }));
  if (busy) for (const control of fields.querySelectorAll("button, input")) control.disabled = true;
  bar.append(el("span"), no, ok);
  box.append(fields, bar);
  return box;
}

function jsonOptionsForm(entry, draft, operations, done) {
  const { edit, save, name, switches, subName } = operations;
  const box = el("div", "pm-opts");
  const ta = document.createElement("textarea");
  ta.setAttribute("aria-label", t("Options"));
  ta.setAttribute("aria-describedby", "pluginOptionsNote pluginOptionsError");
  ta.setAttribute("aria-invalid", String(!!draft.error));
  ta.spellcheck = false;
  ta.value = draft.text;
  ta.rows = Math.min(14, Math.max(4, draft.text.split("\n").length + 1));
  ta.oninput = () => { draft.text = ta.value; };
  const close = () => edit(null);
  const commit = () => {
    let values;
    try { values = JSON.parse(draft.text || "{}"); } catch (err) {
      draft.error = t("Not JSON: {error}", { error: err.message });
      return edit(draft);
    }
    if (!values || typeof values !== "object" || Array.isArray(values)) {
      draft.error = t("Options are a JSON object");
      return edit(draft);
    }
    close();
    save("options", { options: values }, done);
  };
  ta.onkeydown = (ev) => {
    if (ev.key === "Escape") close();
    if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) { ev.preventDefault(); commit(); }
  };
  const err = el("div", "pm-opts-err", draft.error);
  err.id = "pluginOptionsError";
  err.setAttribute("role", "alert");
  const bar = el("div", "pm-opts-bar");
  const note = el("span", "", entry.options ? t("Applied to the next request") : t("Not set: these are its package's example"));
  note.id = "pluginOptionsNote";
  bar.append(note);
  const sw = switches();
  if (sw) {
    const go = el("button", "text quiet pm-opts-switches", t("Its switches are in {name}'s editor", { name: subName(sw) }));
    go.onclick = () => openProvider(sw.id).catch(statusErr);
    bar.append(go);
  }
  if (entry.options) {
    const clear = el("button", "text quiet", t("Clear"));
    clear.title = t("Runs it with no options");
    clear.onclick = async () => {
      if (!await confirmAction(t("Clear"), t("Saved options will be removed. The plugin will run with no options."), t("Clear"))) return;
      close();
      save("options", { options: null }, () => status(t("{name}'s options cleared", { name }), "ok"));
    };
    bar.append(clear);
  }
  const no = el("button", "text", t("Cancel"));
  no.onclick = close;
  const ok = el("button", "text primary", t("Save"));
  ok.onclick = commit;
  bar.append(no, ok);
  box.append(ta, err, bar);
  return box;
}

// what a failed install left in plugins/ that no plugin is installed as:
// Bun installs each again with every plugin, and one it can't install
// failed them all (agency-agents, CHENGJIAHEGOXIS on X). Each can be
// removed here, below Installed's list, empty or not.
window.pmLeftovers = function (body, mine, logo, act, busy) {
  const ls = mine?.leftovers || [];
  if (!ls.length) return;
  const box = el("section", "pm-sec pm-leftovers");
  const h = el("div", "pm-sechead");
  h.append(el("h3", "", t("Not used by any plugin")));
  const list = el("div", "list pm-list");
  for (const x of ls) {
    const r = el("div", "row pm-row");
    r.dataset.leftover = x.name;
    const who = el("div", "who");
    const nm = el("div", "name");
    nm.append(el("span", "", x.name));
    if (x.version) nm.append(el("span", "pm-ver", "v" + x.version));
    who.append(nm);
    const val = el("div", "val");
    const key = "leftover:" + x.name;
    const rm = el("button", "text quiet", busy.get(key) === "leftover" ? t("Removing…") : t("Remove"));
    rm.title = x.spec;
    rm.disabled = busy.size > 0;
    rm.onclick = () => act(key, "leftover", { spec: x.name });
    val.append(rm);
    r.append(logo("", false), who, val);
    list.append(r);
  }
  box.append(h, list);
  body.append(box);
};
