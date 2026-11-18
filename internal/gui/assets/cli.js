// Settings' Command line (PAMI on Discord): whether `magpie` in a terminal
// opened now runs this app, in each shell, and a button that puts it there
// — a link in a folder already on PATH, on Windows the app's folder first
let cliView = null, cliAt = 0, cliAsk = null;
const exeName = (p) => String(p || "").split(/[\\/]/).pop();
window.renderCLI = function (r) {
  r.classList.add("cli-row");
  const sub = r.querySelector(".sub");
  const shells = el("div", "cli-shells");
  const note = el("div", "cli-note");
  r.querySelector(".who").append(shells, note);
  const add = el("button", "text cli-add", t("Add to PATH"));
  add.hidden = true;
  r.querySelector(".val").append(add);
  const go = async (body, btn) => {
    btn.disabled = true;
    try {
      cliView = await api("cli", body);
      cliAt = Date.now();
      paint(cliView);
      status(cliView.windows && cliView.shim ? t("Added: magpie.cmd beside {name} runs it as magpie; open a new PowerShell or Command Prompt window to run magpie", { name: exeName(cliView.exe) })
        : cliView.windows ? t("Added: open a new PowerShell or Command Prompt window to run magpie") : t("Added: open a new terminal window to run magpie"), "ok");
    } catch (e) { statusTErr(e); } finally { btn.disabled = false; }
  };
  const paint = (v) => {
    sub.textContent = v.stuck === "translocated" ? t("macOS runs magpie from a temporary copy until it is moved to Applications: move it there first")
      : v.stuck === "read-only" ? t("magpie runs from its disk image: copy it to Applications first")
      : v.ours ? t("In a new terminal, magpie runs this app: {path}", { path: v.command }) + (/\.(cmd|com)$/i.test(v.command || "") ? " (" + t("which runs {name}", { name: exeName(v.exe) }) + ")" : "")
      : v.command ? t("In a new terminal, magpie runs another copy: {path}", { path: v.command }) + (v.version ? " (" + v.version + ")" : "")
      : t("A new terminal can't find the magpie command");
    add.hidden = v.ours || !v.dir || !!v.stuck;
    add.textContent = v.command ? t("Use this app") : t("Add to PATH");
    // an app not named magpie.exe (the site's magpie-windows-amd64.exe,
    // #942) is given a magpie.cmd beside it that runs it
    add.title = v.windows && v.shim ? t("Puts {dir} first in your user PATH and writes magpie.cmd there, which runs {name}", { dir: v.dir || "", name: exeName(v.exe) })
      : v.windows ? t("Puts {dir} first in your user PATH", { dir: v.dir || "" }) : t("Links magpie in {dir}", { dir: v.dir || "" });
    shells.replaceChildren(...(v.shells || []).map((s) => {
      const line = el("div", "cli-shell" + (s.ours ? " on" : ""));
      line.dataset.shell = s.name;
      line.append(el("span", "cli-dot"), el("span", "cli-sh", s.name + (s.default ? " · " + t("default") : "")),
        el("span", "cli-state", !s.known ? t("its PATH couldn't be read") : s.ours ? t("runs this app")
          : s.command ? t("runs {path}", { path: s.command }) : t("not found")));
      // Add to PATH reaches a shell whose PATH has its folder; one that
      // hasn't gets its own profile's button
      if (!s.ours && s.profile && !v.stuck && !(s.hasDir && !add.hidden)) {
        const b = el("button", "text", t("Add to {file}", { file: s.profile }));
        b.title = t("Adds ~/.local/bin to {file}'s PATH and links magpie there", { file: s.profile });
        b.onclick = () => go({ shell: s.name }, b);
        line.append(b);
      }
      return line;
    }));
    note.textContent = v.windows ? t("PowerShell and cmd take up a change in new windows; restart Windows Terminal if it is open")
      : t("A change reaches terminals opened after it");
  };
  add.onclick = () => go({}, add);
  if (cliView) paint(cliView);
  if (!cliAsk && (!cliView || Date.now() - cliAt > 30000)) {
    cliAsk = api("cli").then((v) => { cliView = v; cliAt = Date.now(); return v; }).finally(() => { cliAsk = null; });
  }
  cliAsk?.then((v) => { if (r.isConnected) paint(v); }, () => { if (!cliView) sub.textContent = t("Couldn't tell"); });
};
