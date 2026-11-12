// Settings › Menu bar windows, and Windows' Tray icon number. app.js imports this only when the menu bar
// shows a card (renderTrayUsage), so it weighs nothing on the core.

// which of a subscription's windows its menu bar card shows, by kind
// (settings.TrayWindows, StrikingRaw on X): a pill each for those ticked
// with two or more, every window unless some are picked. A window the card shows only when picked
// (trayExtra: money aside, one model's, Cursor's Grok Bot, CrazyJal on X)
// is offered too; its usual windows ticked alone are no pick at all.
window.trayWindowsRow = function trayWindowsRow(s, keep, ids, mac) {
  // Windows' tray shows no text: the first card's figure is its icon, or
  // the bird (Benjaminliang_1 on X)
  $("#trayNumberRow").hidden = !document.documentElement.classList.contains("win");
  $("#trayNumberSegs").replaceChildren(segs([["off", t("Off")], ["on", t("On")]], s.trayNoNumber ? "off" : "on",
    (v) => savePrefs({ ...keep, trayNoNumber: v === "off" })));
  const pick = s.trayWindows || {}, box = $("#trayWindowsPick"), subs = [];
  const selected = ids.filter((id) => !id.startsWith("usage:"));
  for (const pid of new Set(selected.map((id) => id.split("|")[0]))) {
    const providerIDs = selected.filter((id) => id.split("|")[0] === pid);
    const inUse = providerIDs.includes(pid + IN_USE);
    const cards = (quotas || []).filter((q) => q.provider === pid && (inUse || providerIDs.includes(trayCardID(q))));
    const ws = [], union = inUse || cards.length > 1;
    for (const q of cards) for (const w of q.windows || []) {
      if (!w.trayKind) continue;
      let option = ws.find((x) => x.v === w.trayKind);
      if (!option) {
        option = { v: w.trayKind, name: w.name, note: "", extra: !!w.trayExtra, accounts: [] };
        ws.push(option);
      } else option.extra = option.extra && !!w.trayExtra;
      const who = [q.alias, q.user].filter(Boolean).join(" · ") || planName(q.plan) || q.name;
      if (union && !option.accounts.includes(who)) option.accounts.push(who);
    }
    for (const w of ws) w.note = w.accounts.join(", ");
    if (ws.length > 1) subs.push([pid, cards[0].name, ws, inUse]);
  }
  // Windows' tray icon draws the first one's figure (traynumber.go), so
  // which window it is matters there too
  $("#trayWindowsRow").hidden = !subs.length;
  if (!quotas && ids.some((id) => !id.startsWith("usage:"))) loadQuotas().then(() => box.isConnected && trayWindowsRow(s, keep, ids, mac));
  box.replaceChildren(...subs.map(([pid, name, ws, inUse]) => {
    const on = (pick[pid] || []).filter((k) => ws.some((w) => w.v === k)), b = el("button", "proto pick" + (on.length ? " set" : ""));
    b.type = "button";
    b.append(el("span", "", name + " · " + (on.length ? on.map((k) => t(ws.find((w) => w.v === k).name)).join(" · ") : t("All windows"))), svg(CHEV, 11, 1.6));
    b.onclick = (e) => {
      e.stopPropagation();
      if (b.classList.contains("open")) return closeProtoMenu();
      openProtoMenu(b, [{ v: "", name: "All windows", note: "" }, ...ws], on, (v) => {
        const next = { ...pick };
        const usual = ws.filter((w) => !w.extra);
        const unseen = (pick[pid] || []).filter((k) => !ws.some((w) => w.v === k));
        if (v.length && !(v.length === usual.length && usual.every((w) => v.includes(w.v)))) next[pid] = [...unseen, ...v];
        else if (v.length && unseen.length) next[pid] = unseen;
        else delete next[pid];
        savePrefs({ ...keep, trayWindows: next });
      }, inUse ? t("Possible windows for the account in use") : "Shown in the menu bar");
    };
    return b;
  }));
};
