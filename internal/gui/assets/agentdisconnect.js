window.agentDisconnectAsk = function (a) {
  const ed = el("div", "editor disconnect-ask");
  const head = el("div", "ehead");
  head.append(icon(a.icon), el("b", "", t("Disconnect {agent} from magpie?", { agent: a.name })));
  ed.append(head);
  ed.append(el("p", "lib-confirm", t("{agent} goes back to how it was before it was connected; these are the places put back.", { agent: a.name })));
  const said = el("p", "lib-confirm", t("magpie takes its endpoint, key, models and effort out of {agent}'s config, restoring what it had. Providers and accounts stay.", { agent: a.name }));
  const body = el("div", "ag-diff-wrap");
  const show = (changes) => {
    if (changes?.length) body.replaceChildren(changesList(changes, 1));
    else body.replaceChildren(said);
  };
  if (previews[a.id]) show(previews[a.id]);
  else body.append(el("div", "ag-diff-loading", t("Reading what changes…")));
  let previewBlocked = !!a.native, revision = "", offline = false;
  loadPreview(a).then((preview) => {
    show(preview.changes);
    revision = preview.revision;
    previewBlocked = !!a.native && !revision;
    go.disabled = previewBlocked;
  }, (err) => { if (!a.native) { show(null); return; } previewBlocked = true; go.disabled = true; body.replaceChildren(el("p", "lib-confirm", t(err.message))); });
  ed.append(body, el("p", "ag-note", t("The Library's MCP servers, skills and hooks stay")));
  if (kindOf(a) === "codex") ed.append(el("p", "ag-note", t("magpie's provider table stays, so sessions opened on magpie's models still open")));
  const bar = el("div", "bar");
  const go = el("button", "text primary danger-fill", t("Disconnect and restore"));
  go.disabled = previewBlocked;
  go.onclick = async (e) => {
    e.stopPropagation();
    if (previewBlocked) return;
    go.disabled = true;
    go.classList.add("busy");
    try {
      state = await api("agents/" + (offline ? "disconnect-offline/" : "disconnect/") + a.id, offline ? { revision } : {});
      closeConfirmAsk();
      delete previews[a.id];
      if (agentExpanded === a.id) agentExpanded = null;
      renderAgents();
      const msg = t(offline ? "Saved settings restored; magpie was removed from {agent}" : "{agent} no longer goes through magpie; its own settings are back", { agent: a.name });
      saidOk(msg);
    } catch (err) {
      go.disabled = false;
      go.classList.remove("busy");
      if (!offline && err.code === "runtime_unavailable" && err.offline === "disconnect" && revision) {
        offline = true;
        head.querySelector("b").textContent = t("Restore saved settings and remove magpie?");
        ed.querySelector(".lib-confirm").textContent = t("Restore saved settings and remove magpie without contacting Aside? Close Aside first if it is running.");
        go.textContent = t("Restore saved settings and remove magpie");
        cancel.focus(NO_SCROLL);
        return;
      }
      status(t(err.message), "err");
    }
  };
  const cancel = el("button", "text", t("Cancel"));
  cancel.onclick = cancelAsk;
  bar.append(reopenNote(a), el("span", "grow"), cancel, go);
  ed.append(bar);
  confirmAsk = ed;
  openModal(ed);
  $("#modal").classList.add("lib");
  cancel.focus(NO_SCROLL);
};
