// Settings › Network's Update source (RootSir and Sakugar_ on X): where an
// update is read and downloaded from, one menu button as the language's is.
// app.js imports this when the page is drawn, so it weighs nothing on the
// core. "" is automatic; site never goes to GitHub, github never to
// usemagpie.ai.
window.updateSourcePick = function updateSourcePick(s, keep) {
  const names = { "": "Automatic", site: "usemagpie.ai only", github: "GitHub only" };
  const b = $("#updateSourcePick");
  const chosen = s.updateSource in names ? s.updateSource : "";
  b.dataset.value = chosen;
  b.setAttribute("aria-label", t("Update source"));
  b.replaceChildren(el("span", "", t(names[chosen])), svg(CHEV, 11, 1.6));
  updateLocalSigning().catch(statusErr);
  b.onclick = (e) => {
    e.stopPropagation();
    if (b.classList.contains("open")) return closeProtoMenu();
    openProtoMenu(b, Object.entries(names).map(([v, name]) => ({ v, name, note: "" })), chosen, (v) => {
      if (v !== chosen) savePrefs({ ...keep, updateSource: v });
    }, "Update source", "sess-menu", "right");
  };
};

let updateSigningDraw = 0;
async function updateLocalSigning() {
  const source = $("#updateSourceRow");
  if (!source) return;
  const draw = ++updateSigningDraw;
  const data = await api("update/signing");
  if (draw !== updateSigningDraw || !source.isConnected) return;
  $("#updateSigningRow")?.remove();
  if (!data?.supported) return;
  const row = el("div", "row pref"), who = el("div", "who");
  row.id = "updateSigningRow";
  who.append(el("div", "name", t("Local update signing")), el("div", "sub", t("Sign verified updates with a certificate on this Mac")));
  const pick = el("button", "sess-pick pref-pick");
  pick.type = "button";
  pick.id = "updateSigningPick";
  pick.setAttribute("aria-label", t("Local update signing"));
  pick.setAttribute("aria-haspopup", "menu");
  pick.setAttribute("aria-expanded", "false");
  const paint = (s) => {
    const label = s.configured ? s.fingerprint.slice(-8) : t("Off");
    pick.replaceChildren(el("span", "", label), svg(CHEV, 11, 1.6));
    pick.title = s.configured ? `${s.fingerprint} · ${s.team} · ${s.feed}` : t("Local update signing");
  };
  paint(data);
  pick.onclick = async (e) => {
    e.stopPropagation();
    if (pick.classList.contains("open")) return closeProtoMenu();
    pick.disabled = true;
    try {
      const s = await api("update/signing?identities=1");
      if (!row.isConnected) return;
      const choices = [{ v: "", name: t("Off"), note: "" }, ...(s.identities || []).map((id) => ({
        v: id.fingerprint, name: `${id.name} · ${id.fingerprint.slice(-8)}`, note: `${id.team} · ${id.fingerprint}`,
      }))];
      if (s.configured && !choices.some((c) => c.v === s.fingerprint)) choices.push({ v: s.fingerprint, name: s.fingerprint, note: t("Signing certificate unavailable") });
      openProtoMenu(pick, choices, s.configured ? s.fingerprint : "", (v) => {
        if (v === (s.configured ? s.fingerprint : "")) return;
        pick.disabled = true;
        api("update/signing", v ? { fingerprint: v } : { off: true }).then((next) => {
          paint(next);
          status(t("Saved"));
        }, statusErr).finally(() => { pick.disabled = false; });
      }, "Local update signing", "sess-menu", "right");
    } catch (err) { statusErr(err); }
    finally { pick.disabled = false; }
  };
  row.append(who, pick);
  source.after(row);
}
