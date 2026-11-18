function previewForm(draft) {
  // many keys pasted (361 on Discord) are asked about by the first
  const keys = splitKeys(draft.key || "");
  const body = { typed: true, key: keys.length > 1 ? keys[0] : (draft.key || "").trim(), chat: (draft.chat || "").trim(), responses: (draft.responses || "").trim(), anthropic: (draft.anthropic || "").trim(), gemini: (draft.gemini || "").trim(), modelsURL: (draft.modelsURL || "").trim() };
  Object.assign(body, movedURLs(draft));
  // a System One base is asked at POST …/systemone, not on the three APIs
  // and one given beside a conversation URL, under More endpoints
  if (draft.api === "decide" || !draft.preset && (draft.decide || "").trim()) body.decide = (draft.decide || "").trim();
  // a Vertex AI provider is asked at the project typed, with the
  // credentials typed, as a pasted key is
  if (draft.vertex) body.vertex = vertexOfDraft();
  if (draft.headers) body.headers = headersOf(draft.headers);
  // the extra body typed, when it reads; one that doesn't is the saved one's
  const xb = extraBodyOf(draft.extraBody);
  if (xb.value) body.extraBody = xb.value;
  const proxy = draft.proxyMode === undefined ? null : proxyOfDraft();
  if (proxy !== null) body.proxy = proxy;
  // asked unchecked as it would be saved; magpie takes it only for a
  // provider of the user's own
  body.insecureTLS = !!draft.insecureTLS;
  body.http1 = !!draft.http1;
  // a client certificate typed (#5596) and connect addresses are tried as they would be saved
  window.tlsTyped?.(draft, body);
  return body;
}

export function providerPreview(root, owner, provider, live, replace) {
  let host = root, source = provider, field = null, ref = "", value = "", saving = null;
  let retired = false, suspended = false, invalid = false, menuAnchor = null, events = null, continuing = null;
  const active = () => !retired && !suspended && live() && host.isConnected;
  const control = (node) => {
    for (let at = node; at && at !== host.parentElement; at = at.parentElement) {
      if (at.previewDraft === owner && host.contains(at)) return at;
    }
    return null;
  };
  const inMenu = (node) => [agentMenu, protoMenu].some((menu) => menu && menu.anchor === menuAnchor && menu.box.contains(node));
  const remember = () => { if (field) value = field.value; };
  const draftChanged = () => host.dispatchEvent(new Event("provider-draft-change"));
  const clear = () => {
    if (field) field.value = "";
    field = null; ref = value = ""; invalid = false; menuAnchor = null;
    draftChanged();
  };
  const mount = () => {
    if (!ref || saving) return;
    const account = source?.keyList?.find((key) => key.id === ref);
    const row = host.querySelector(`.accts.keys .acc[data-account-id="${CSS.escape(ref)}"]`);
    if (!account || !row) { field = null; value = ""; invalid = true; draftChanged(); return; }
    const previous = field;
    const next = input(value, t("Paste the key's new value"), "password");
    next.className = "rename-in key-value-in";
    field = next;
    next.oninput = () => { if (field === next) value = next.value; };
    next.onkeydown = (event) => {
      event.stopPropagation();
      if (event.key === "Enter") api.commit();
      else if (event.key === "Escape") { clear(); renderProviders(); }
    };
    next.onblur = (event) => {
      if (field !== next || !active() || saving || control(event.relatedTarget) || inMenu(event.relatedTarget)) return;
      if (next.value.trim()) api.commit();
      else { clear(); renderProviders(); }
    };
    if (previous?.isConnected && host.contains(previous)) previous.replaceWith(next);
    else {
      const shown = row.querySelector(".plan.mono");
      if (shown) shown.replaceWith(next);
      else row.querySelector(".n.rename").after(next);
    }
  };
  const api = {
    matches: (draft, id) => !retired && owner === draft && source?.id === id,
    hasPendingReplacement: () => !!ref && !saving && !invalid && !!(field?.value ?? value).trim(),
    suspend() { remember(); suspended = true; events?.abort(); },
    resume() { api.attach(host, source); field?.focus(NO_SCROLL); },
    remount() { remember(); mount(); },
    retire() { retired = true; events?.abort(); clear(); },
    attach(next, current) {
      remember(); host = next; source = current; suspended = false;
      host.previewOwner = api;
      events?.abort();
      events = new AbortController();
      const options = { capture: true, signal: events.signal };
      host.addEventListener("pointerdown", (event) => {
        if (ref && active() && control(event.target)) event.preventDefault();
      }, options);
      document.addEventListener("focusin", (event) => {
        if (ref && active() && !saving && event.target !== field && !control(event.target) && !inMenu(event.target)) api.commit();
      }, options);
      mount();
    },
    edit(row, key) {
      if (!active() || saving || row.querySelector(".key-value-in")) return;
      clear(); ref = key.id;
      mount();
      field?.focus(NO_SCROLL);
    },
    menu(anchor) { menuAnchor = anchor; },
    body(account) {
      if (!active() || saving || invalid) throw new Error(t("Test stopped"));
      remember();
      const body = previewForm(owner);
      if (ref && (!account || account === ref)) {
        if (!source?.keyList?.some((key) => key.id === ref && key.on) || !value.trim()) throw new Error(t("Paste the key's new value"));
        body.key = value.trim();
        body.typedKeyRef = ref;
      } else if (ref) body.key = "";
      return body;
    },
    finish(run) {
      if (continuing) return continuing;
      continuing = Promise.resolve().then(async () => {
        if (!active() || !await api.commit() || !active()) return false;
        return api.run(run);
      }).finally(() => { continuing = null; });
      return continuing;
    },
    run: (run) => active() && run(host, source),
    commit() {
      if (saving) return saving;
      if (invalid || !active()) return Promise.resolve(false);
      if (!ref) return Promise.resolve(true);
      remember();
      if (!value.trim()) { clear(); return Promise.resolve(true); }
      const body = { id: source.id, ref, key: value };
      const key = source.keyList.find((key) => key.id === ref);
      const previous = source.keyList.map((key) => key.id), slot = previous.indexOf(ref);
      let accepted = false;
      saving = Promise.resolve(replace(body, t("{key} now uses its new value", { key: key?.name || key?.masked }), (current) => {
        const next = current?.keyList?.[slot]?.id;
        if (retired || !live() || current?.id !== source.id || !next || current.keyList.length !== previous.length ||
          previous.some((id, index) => index !== slot && current.keyList[index].id !== id)) return;
        for (const map of [owner.accountProxies, owner.keyPriceRates]) if (next !== ref && map && Object.hasOwn(map, ref)) {
          map[next] = map[ref];
          delete map[ref];
        }
        if (acctModels?.id === source.id && acctModels.ref === ref) acctModels.ref = next;
        ref = next;
        accepted = true;
      })).then((ok) => {
        if (ok) clear();
        return ok && accepted && active();
      }).finally(() => {
        saving = null;
        if (active()) { mount(); draftChanged(); }
      });
      draftChanged();
      return saving;
    },
  };
  api.attach(root, provider);
  return api;
}
