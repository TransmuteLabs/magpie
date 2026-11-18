// Settings › About's Update source (RootSir and Sakugar_ on X): where an
// update is read and downloaded from, one menu button as the language's is.
// app.js imports this when the page is drawn, so it weighs nothing on the
// core. "" is automatic; site never goes to GitHub, github never to
// usemagpie.ai.
export function renderSources(mirror, source, s, keep) {
  if (!source.isConnected) return;
  renderUpdateMirror(mirror, s);
  aboutCommunity();
  const names = { "": "Automatic", site: "usemagpie.ai only", github: "GitHub only" };
  const b = el("button", "sess-pick pref-pick");
  Object.assign(b, { type: "button", id: "updateSourcePick" });
  menuOf(b);
  source.querySelector(".val").append(b);
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
}

const GITHUB_SVG = '<svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor" aria-hidden="true"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>';
const DISCORD_SVG = '<svg viewBox="0 0 24 24" width="13" height="13" fill="currentColor" aria-hidden="true"><path d="M20.317 4.3698a19.7913 19.7913 0 00-4.8851-1.5152.0741.0741 0 00-.0785.0371c-.211.3753-.4447.8648-.6083 1.2495-1.8447-.2762-3.68-.2762-5.4868 0-.1636-.3933-.4058-.8742-.6177-1.2495a.077.077 0 00-.0785-.037 19.7363 19.7363 0 00-4.8852 1.515.0699.0699 0 00-.0321.0277C.5334 9.0458-.319 13.5799.0992 18.0578a.0824.0824 0 00.0312.0561c2.0528 1.5076 4.0413 2.4228 5.9929 3.0294a.0777.0777 0 00.0842-.0276c.4616-.6304.8731-1.2952 1.226-1.9942a.076.076 0 00-.0416-.1057c-.6528-.2476-1.2743-.5495-1.8722-.8923a.077.077 0 01-.0076-.1277c.1258-.0943.2517-.1923.3718-.2914a.0743.0743 0 01.0776-.0105c3.9278 1.7933 8.18 1.7933 12.0614 0a.0739.0739 0 01.0785.0095c.1202.099.246.1981.3728.2924a.077.077 0 01-.0066.1276 12.2986 12.2986 0 01-1.873.8914.0766.0766 0 00-.0407.1067c.3604.698.7719 1.3628 1.225 1.9932a.076.076 0 00.0842.0286c1.961-.6067 3.9495-1.5219 6.0023-3.0294a.077.077 0 00.0313-.0552c.5004-5.177-.8382-9.6739-3.5485-13.6604a.061.061 0 00-.0312-.0286zM8.02 15.3312c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9555-2.4189 2.157-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.9555 2.4189-2.1569 2.4189zm7.9748 0c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9554-2.4189 2.1569-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.946 2.4189-2.1568 2.4189Z"/></svg>';
window.aboutCommunity = function aboutCommunity() {
  for (const [id, label, run] of [
    ["configFolderRow", "Open", () => api("settings/reveal", {}).catch(statusErr)],
    ["logRow", "Open", () => api("settings/log/reveal", {}).catch(statusErr)],
    ["websiteRow", "Open", () => api("open", { url: "https://usemagpie.ai" }).catch(() => {})],
    ["licensesRow", "View", showNotices],
  ]) {
    const box = $("#" + id + " .val");
    if (!box) continue;
    box.querySelector(".about-open")?.remove();
    const button = el("button", "text about-open", t(label));
    button.onclick = run;
    box.append(button);
  }
  const val = $("#communityRow .val");
  if (!val) return;
  const join = el("button", "discord");
  join.innerHTML = DISCORD_SVG;
  join.append(el("span", "", t("Join Discord")));
  join.title = "discord.gg/vGSnD3ZKQF";
  join.onclick = () => api("open", { url: "https://discord.gg/vGSnD3ZKQF" }).catch(() => {});
  const repo = el("button", "github");
  repo.innerHTML = GITHUB_SVG;
  repo.append(el("span", "", "GitHub"));
  repo.title = "github.com/yetone/magpie";
  repo.onclick = () => api("open", { url: "https://github.com/yetone/magpie" }).catch(() => {});
  val.replaceChildren(join, repo);
  const reportVal = $("#reportProblemRow .val");
  if (!reportVal) return;
  const report = el("button", "text", t("Open"));
  report.onclick = async () => {
    const f = await fetch("/api/feedback?surface=" + (web ? "browser" : "app")).then((r) => r.ok ? r.json() : null).catch(() => null);
    if (!f) return status(t("Couldn't read the details to copy"), "err");
    await copy(f.text, "", report, t("Details copied. Paste them in your reply"));
    api("open", { url: f.url }).catch(statusErr);
  };
  reportVal.replaceChildren(report);
};

// Settings' Download source, drawn here with Update source so it weighs
// nothing on the core; it says what the source picked does
window.setUpdateMirror = function (mirror) {
  return writingPrefs(api("settings/update-mirror", { mirror }))
    .then((ns) => { prefs = ns; mirrorEditing = false; mirrorDraft = mirrorErr = ""; renderSettings(); status(t("Saved"), "ok", 1500); return ns; });
};
// Use a mirror… and Change open the field from mirroredit.js
const editMirror = (r, s, v) => import("./mirroredit.js").then(() => { mirrorEditing = true; mirrorDraft = v; renderUpdateMirror(r, s); }, statusErr);
window.renderUpdateMirror = function (r, s) {
  r.classList.add("update-mirror-row");
  const who = r.querySelector(".who"), val = r.querySelector(".val");
  who.querySelector(".sub")?.remove();
  val.replaceChildren();
  const sub = el("div", "sub");
  who.append(sub);
  const m = s.updateMirror || "", from = s.updateSource;
  // as Update source says: site never asks GitHub, so no mirror is used
  sub.textContent = mirrorErr || (from === "site" ? t("Update source is usemagpie.ai only: updates never come from GitHub, so no mirror is used")
    : from === "github" ? (m ? t("Update source is GitHub only: updates come through this mirror, each checked against the release's signed checksums") : t("Update source is GitHub only: updates come from GitHub; a mirror can stand in for it where it is slow"))
    : m ? t("Updates are downloaded from usemagpie.ai, and through this mirror when that fails, each checked against usemagpie.ai's checksum")
    : t("Updates are downloaded from usemagpie.ai, and from GitHub when that fails; a mirror can stand in for GitHub where it is slow"));
  // the docs' Mirrors page says which mirror and how (WeltAn_X on X)
  const more = el("button", "sub-more", t("How mirrors work"));
  more.onclick = () => api("open", { url: "https://usemagpie.ai/docs/" + ({ zh: "zh/", "zh-TW": "zh/", ja: "ja/" }[locale] || "") + "mirrors#settings" }).catch(() => {});
  sub.append(" ", more);
  sub.classList.toggle("err", !!mirrorErr);
  sub.title = from === "github" ? t("The checksums come signed with the release; a file from the mirror that doesn't match them is not installed.")
    : t("The update and its checksum always come from usemagpie.ai; a file from the mirror that doesn't match it is not installed.");
  if (mirrorEditing) return mirrorEdit(r, s, sub, val);
  if (m) {
    const official = el("button", "text", t("Remove mirror"));
    official.onclick = () => setUpdateMirror("").catch(statusTErr);
    const change = el("button", "text", t("Change"));
    change.onclick = () => editMirror(r, s, m);
    val.append(el("code", "", m), change, official);
  } else {
    const add = el("button", "text", t("Use a mirror…"));
    add.onclick = () => editMirror(r, s, "");
    val.append(el("code", "", from === "github" ? "github.com" : "usemagpie.ai"), add);
  }
};
let updateSigningDraw = 0;
async function updateLocalSigning() {
  const source = $("#about .update-mirror-row");
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
