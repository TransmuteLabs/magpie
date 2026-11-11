// The coordinator owns execution claims; this panel only asks for explicit
// operations and displays the account-scoped results it receives.
export function zcodeExecutionPanel(c) {
  const ui = magpie.ui;
  const root = ui.el("div", "zcode-execution");
  const encoder = new TextEncoder();
  const fixedControls = [];
  let recordControls = [];
  let records = null, listKnown = false, listAttempted = false, nextDrawAt;
  let reading = false, mutating = false, model = "GLM-5.3";

  const stateNames = {
    queued: t("Queued"), ready: t("Ready"), active: t("Active"), expired: t("Expired"),
    settled: t("Settled"), not_found: t("Not found"), pending: t("Pending"),
    settling: t("Settling"), unknown: t("Unknown"),
  };
  const laneName = (lane) => lane === "offpeak" ? t("Off-Peak") : lane === "highspeed" ? t("Highspeed") : value(lane);
  const value = (v) => v === undefined || v === null || v === "" ? t("Unknown") : String(v);
  const bool = (v) => v === true ? t("Yes") : v === false ? t("No") : t("Unknown");
  const stateName = (v) => stateNames[v] || value(v);
  const time = (v) => typeof v !== "number" || !Number.isFinite(v) ? t("Unknown")
    : v > 0 ? c.formatTime(v, "ms") : String(v);
  const cooldown = (v) => v === 0 ? t("No cooldown recorded") : time(v);
  const validTask = (v) => typeof v === "string" && v !== "" && v === v.trim() && !/^\u0085|\u0085$/.test(v)
    && !/[\x00-\x1f\x7f]/.test(v) && encoder.encode(v).length <= 128;

  // A selector is supplied by the owner, not reconstructed from the auth slot.
  // The gateway accepts an encoded selector, including a non-ASCII name.
  const accountHeader = () => {
    const s = c.accountSelector;
    if (typeof s !== "string" || !s || s !== s.trim() || /[\x00-\x1f\x7f]/.test(s)) return "";
    try { return /[^\x20-\x7e]/.test(s) ? encodeURIComponent(s) : s; } catch { return ""; }
  };
  const blocked = () => !c.alive() || !!c.busy || reading || mutating;
  const mayMutate = () => !blocked() && c.canMutate() && listKnown;
  function addButton(group, label, onClick, enabled, mutation = false) {
    const b = ui.button(t(label), onClick, "text");
    if (mutation) b.dataset.zcodeMutation = "";
    group.push({ b, enabled, mutation });
    return b;
  }
  function syncControls() {
    for (const { b, enabled, mutation } of [...fixedControls, ...recordControls]) {
      b.disabled = blocked() || (mutation && !c.canMutate()) || !enabled();
    }
    taskInput.disabled = mutating || !!c.busy;
    modelMenu.disabled = mutating || !!c.busy;
    ticketInput.disabled = mutating || !!c.busy;
    cardInput.disabled = mutating || !!c.busy;
  }
  function section(title, description) {
    const s = ui.el("section", "zcode-section");
    s.append(ui.el("h3", "", t(title)));
    if (description) s.append(ui.el("p", "hint", t(description)));
    root.append(s);
    return s;
  }
  function kv(parent, label, v) {
    const row = ui.el("div", "zcode-kv");
    row.append(ui.el("span", "", t(label)), ui.el("span", "", v));
    parent.append(row);
  }
  function details(parent, e) {
    kv(parent, "Task ID", value(e.taskId));
    kv(parent, "Lane", laneName(e.lane));
    kv(parent, "Model", value(e.rawModel));
    if (["GLM-5.3", "GLM-5.3-Flash"].includes(e.rawModel) && ["offpeak", "highspeed"].includes(e.lane)) {
      kv(parent, e.lane === "offpeak" ? "Off-Peak model alias" : "Highspeed model alias", e.rawModel + (e.lane === "offpeak" ? "-OffPeak" : "-Highspeed"));
    }
    kv(parent, "State", stateName(e.state));
    if (e.lane === "offpeak") {
      kv(parent, "Ticket ID", value(e.ticketId));
      kv(parent, "Ready deadline", time(e.readyDeadline));
      kv(parent, "Active deadline", time(e.expiresAt));
    } else if (e.lane === "highspeed") {
      kv(parent, "Card ID", value(e.cardId));
      kv(parent, "Card expiry", time(e.expiresAt));
      if (e.nextDrawAt !== undefined) kv(parent, "Next Highspeed draw", cooldown(e.nextDrawAt));
    }
    kv(parent, "Resolution required", bool(e.requiresResolution));
  }

  const intro = section("Execution handoff", "No admission, draw, continuation or settlement happens automatically. Use the same conversation ID and account for inference.");
  const fields = ui.el("div", "zcode-fields");
  const taskInput = ui.el("input", "");
  taskInput.type = "text";
  taskInput.autocomplete = "off";
  taskInput.spellcheck = false;
  fields.append(...field(t("Conversation task ID"), taskInput, t("Enter the exact conversation ID for X-Magpie-Session: no leading or trailing whitespace, no control bytes, and at most 128 UTF-8 bytes.")));
  const modelMenu = ui.menu([
    { v: "GLM-5.3", name: "GLM-5.3" },
    { v: "GLM-5.3-Flash", name: "GLM-5.3-Flash" },
  ], model, t("Model"), (v) => { model = v; drawHandoff(); syncControls(); });
  modelMenu.setAttribute("aria-label", t("Model"));
  fields.append(...field(t("Model"), modelMenu));
  intro.append(fields);
  const handoff = ui.el("div", "zcode-list");
  intro.append(handoff);
  function drawHandoff() {
    handoff.replaceChildren();
    const selector = accountHeader();
    if (!selector) handoff.append(ui.el("p", "zcode-error", t("The account routing selector is unavailable. Admission is disabled because requests cannot be pinned to this account.")));
    else kv(handoff, "X-Magpie-Account", selector);
    kv(handoff, "X-Magpie-Session", validTask(taskInput.value) ? taskInput.value : t("Enter a valid conversation ID first"));
    kv(handoff, "Off-Peak model alias", model + "-OffPeak");
    kv(handoff, "Highspeed model alias", model + "-Highspeed");
    handoff.append(ui.el("p", "hint", t("Use the model ID reported by your gateway; it may need a provider prefix. A queued ticket is not ready until status says so.")));
  }

  const actions = ui.el("div", "zcode-actions");
  intro.append(actions);
  const taskAvailable = () => validTask(taskInput.value) && !records?.some((r) => r.taskId === taskInput.value);
  actions.append(addButton(fixedControls, "Take Off-Peak ticket", () => {
    const taskId = taskInput.value, selectedModel = model;
    if (!validTask(taskId)) return;
    mutate("Take Off-Peak ticket", t("Take an Off-Peak ticket for {task} with {model}? Admission can consume an opportunity. If the reply is lost, inspect the task before doing anything else.", { task: taskId, model: selectedModel }), "execution.take", { taskId, model: selectedModel });
  }, () => mayMutate() && !!accountHeader() && taskAvailable(), true));
  actions.append(addButton(fixedControls, "Draw Highspeed card", () => {
    const taskId = taskInput.value, selectedModel = model;
    if (!validTask(taskId)) return;
    mutate("Draw Highspeed card", t("Draw a Highspeed card for {task} with {model}? The draw can start a cooldown even if no card is returned. Do not retry an unknown outcome.", { task: taskId, model: selectedModel }), "execution.draw", { taskId, model: selectedModel });
  }, () => mayMutate() && !!accountHeader() && taskAvailable()
    && !(typeof nextDrawAt === "number" && nextDrawAt > Date.now())
    && !records?.some((r) => r.lane === "highspeed" && (r.state === "pending" || r.state === "unknown")), true));
  taskInput.addEventListener("input", () => {
    taskInput.setAttribute("aria-invalid", String(taskInput.value !== "" && !validTask(taskInput.value)));
    drawHandoff();
    syncControls();
  });

  const availabilitySection = section("Off-Peak availability", "This snapshot is for new tickets, not the usability of an existing ticket. Admission remains the final authority.");
  const availabilityActions = ui.el("div", "zcode-actions");
  availabilitySection.append(availabilityActions);
  availabilityActions.append(addButton(fixedControls, "Check availability", () => runRead("offpeak.availability", {}, (data) => {
    availabilityResult.replaceChildren();
    kv(availabilityResult, "Can take a new ticket", bool(data?.can_take_number));
    if (data?.can_take_number === false || data?.next_take_at !== undefined) kv(availabilityResult, "Next ticket availability", time(data?.next_take_at));
  }), () => true));
  const availabilityResult = ui.el("div", "zcode-list");
  availabilitySection.append(availabilityResult);
  kv(availabilityResult, "Can take a new ticket", t("Unknown"));

  const history = section("Known executions", "Refresh reads the local coordinator. Status is a separate explicit read; an unknown outcome is never retried automatically.");
  const historyActions = ui.el("div", "zcode-actions");
  const refresh = addButton(fixedControls, "Refresh", () => loadList(), () => true);
  refresh.dataset.zcodeRefresh = "";
  historyActions.append(refresh);
  history.append(historyActions);
  const list = ui.el("div", "zcode-list");
  history.append(list);
  list.append(ui.el("p", "zcode-empty", t("Reading…")));

  const resultSection = section("Last execution result", "Read results are observations, not a promise that a ticket or card is usable.");
  const resultBox = ui.el("div", "zcode-list");
  resultSection.append(resultBox);
  resultBox.append(ui.el("p", "zcode-empty", t("No result yet")));

  const inspect = section("Read-only inspectors", "Inspect exact vendor IDs for this account. These reads do not admit, draw or settle a task.");
  const inspectFields = ui.el("div", "zcode-fields");
  const ticketInput = ui.el("textarea", "");
  ticketInput.rows = 3;
  ticketInput.spellcheck = false;
  inspectFields.append(...field(t("Ticket IDs"), ticketInput, t("Enter one exact ticket ID per line, up to 100. IDs are sent exactly as typed.")));
  const cardInput = ui.el("input", "");
  cardInput.type = "text";
  cardInput.autocomplete = "off";
  cardInput.spellcheck = false;
  inspectFields.append(...field(t("Card ID"), cardInput));
  inspect.append(inspectFields);
  const inspectActions = ui.el("div", "zcode-actions");
  inspect.append(inspectActions);
  const ticketIDs = () => ticketInput.value === "" ? [] : ticketInput.value.split("\n");
  const validTickets = () => {
    const ids = ticketIDs();
    return ids.length > 0 && ids.length <= 100 && ids.every((id) => id.trim() !== "");
  };
  inspectActions.append(addButton(fixedControls, "Inspect tickets", () => {
    if (!validTickets()) return;
    runRead("offpeak.status", { ticket_ids: ticketIDs() }, showTickets);
  }, validTickets));
  inspectActions.append(addButton(fixedControls, "Inspect card metrics", () => {
    const cardId = cardInput.value;
    if (!cardId.trim()) return;
    runRead("highspeed.healthy", { card_id: cardId }, showMetrics);
  }, () => cardInput.value.trim() !== ""));
  ticketInput.addEventListener("input", () => {
    ticketInput.setAttribute("aria-invalid", String(ticketInput.value !== "" && !validTickets()));
    syncControls();
  });
  cardInput.addEventListener("input", syncControls);

  function showTickets(data) {
    resultBox.replaceChildren(ui.el("b", "", t("Ticket inspection")));
    if (data?.next_poll_after !== undefined) kv(resultBox, "Suggested next poll (seconds)", value(data.next_poll_after));
    if (!Array.isArray(data?.tickets)) {
      resultBox.append(ui.el("p", "zcode-error", t("Ticket inspection is unavailable")));
      return;
    }
    if (!data.tickets.length) resultBox.append(ui.el("p", "zcode-empty", t("No tickets returned")));
    for (const ticket of data.tickets) {
      const item = ui.el("div", "zcode-list");
      kv(item, "Ticket ID", value(ticket.ticket_id));
      if (ticket.task_id !== undefined) kv(item, "Task ID", value(ticket.task_id));
      kv(item, "State", stateName(ticket.state));
      kv(item, "Queue position", value(ticket.position));
      kv(item, "Active deadline", time(ticket.active_deadline));
      resultBox.append(item);
    }
  }
  function showMetrics(data) {
    resultBox.replaceChildren(ui.el("b", "", t("Highspeed card metrics")));
    kv(resultBox, "Card ID", value(data?.card_id));
    kv(resultBox, "Prompt tokens", value(data?.prompt_tokens));
    kv(resultBox, "Completion tokens", value(data?.completion_tokens));
    kv(resultBox, "Duration (seconds)", value(data?.duration_seconds));
  }
  function showStatus(data) {
    resultBox.replaceChildren(ui.el("b", "", t("Execution status")));
    const e = data?.execution && typeof data.execution === "object" ? data.execution : data;
    if (!e || typeof e !== "object") {
      resultBox.append(ui.el("p", "zcode-error", t("Execution status is unavailable")));
      return;
    }
    details(resultBox, { ...e, requiresResolution: data?.requiresResolution ?? e.requiresResolution });
    if (data?.observedState !== undefined) kv(resultBox, "Observed vendor state", stateName(data.observedState));
  }
  function showAction(operation, data) {
    resultBox.replaceChildren(ui.el("b", "", t("Last execution result")));
    if (operation === "execution.take" || operation === "execution.continue") {
      kv(resultBox, "Ticket ID", value(data?.ticket_id));
      if (data?.task_id !== undefined) kv(resultBox, "Task ID", value(data.task_id));
      kv(resultBox, "State", stateName(data?.state));
      kv(resultBox, "Accepted", bool(data?.accepted));
      kv(resultBox, "Queue position", value(data?.position));
      kv(resultBox, "Queued at", time(data?.queued_at));
      kv(resultBox, "Ready deadline", time(data?.ready_deadline));
      if (data?.next_poll_after !== undefined) kv(resultBox, "Suggested next poll (seconds)", value(data.next_poll_after));
    } else if (operation === "execution.draw") {
      if (data?.card === null) resultBox.append(ui.el("p", "zcode-empty", t("No Highspeed card was drawn")));
      else if (data?.card && typeof data.card === "object") {
        const card = data.card;
        kv(resultBox, "Card ID", value(card.card_id));
        kv(resultBox, "Task ID", value(card.task_id));
        kv(resultBox, "Model", value(card.model));
        kv(resultBox, "Issued at", time(card.issued_at));
        kv(resultBox, "Card expiry", time(card.expires_at));
      } else resultBox.append(ui.el("p", "zcode-error", t("The draw result is unavailable")));
      kv(resultBox, "Next Highspeed draw", cooldown(data?.next_draw_at));
    } else {
      resultBox.append(ui.el("p", "", t("Execution request completed")));
      if (data?.nextDrawAt !== undefined) kv(resultBox, "Next Highspeed draw", cooldown(data.nextDrawAt));
    }
  }
  function drawRecords() {
    recordControls = [];
    list.replaceChildren();
    kv(list, "Next Highspeed draw", cooldown(nextDrawAt));
    if (records && !listKnown) list.append(ui.el("p", "zcode-error", t("Execution list unavailable") + " · " + t("Refresh")));
    if (!records) list.append(ui.el("p", "zcode-empty", t("Execution list unavailable")));
    else if (!records.length) list.append(ui.el("p", "zcode-empty", t("No known executions")));
    else for (const record of records) {
      const item = ui.el("div", "zcode-list");
      details(item, record);
      const controls = ui.el("div", "zcode-actions");
      item.append(controls);
      const status = addButton(recordControls, "Refresh status", () => runRead("execution.status", { taskId: record.taskId }, showStatus, true), () => listKnown);
      status.dataset.zcodeRefresh = "";
      controls.append(status);
      if (record.lane === "highspeed" && record.cardId) {
        controls.append(addButton(recordControls, "Read card metrics", () => runRead("highspeed.healthy", { card_id: record.cardId }, showMetrics), () => listKnown));
      }
      if (record.requiresResolution) {
        controls.append(addButton(recordControls, "Resolve uncertainty", () => mutate("Resolve uncertainty", t("Forget the uncertain execution for {task}? The vendor may already have admitted or settled it. Check the vendor first; forgetting can allow another admission or draw.", { task: record.taskId }), "execution.resolve", { taskId: record.taskId, resolution: "forget" }), () => mayMutate(), true));
      } else {
        for (const [label, operation] of [["Finish execution", "execution.finish"], ["Cancel execution", "execution.cancel"]]) {
          controls.append(addButton(recordControls, label, () => mutate(label, t("End the native binding for task {task}? This does not stop the conversation. Off-Peak settles its ticket; Highspeed only clears its local binding and keeps the cooldown.", { task: record.taskId }), operation, { taskId: record.taskId }), () => mayMutate(), true));
        }
        if (record.lane === "offpeak" && record.state === "expired") {
          controls.append(addButton(recordControls, "Continue expired Off-Peak", () => {
            if (!validTask(record.taskId)) return;
            mutate("Continue expired Off-Peak", t("Take a replacement ticket for expired Off-Peak task {task}? This can consume an opportunity; do not retry an unknown outcome.", { task: record.taskId }), "execution.continue", { taskId: record.taskId, model: record.rawModel });
          }, () => mayMutate() && !!accountHeader() && validTask(record.taskId) && ["GLM-5.3", "GLM-5.3-Flash"].includes(record.rawModel), true));
        }
      }
      list.append(item);
    }
    syncControls();
  }
  async function readList() {
    const result = await c.call("execution.list", {});
    if (!c.alive()) return;
    if (!Array.isArray(result?.data?.executions)) throw new Error(t("Execution list unavailable"));
    records = result.data.executions;
    nextDrawAt = result.data.nextDrawAt;
    listKnown = true;
    drawRecords();
  }
  async function loadList() {
    if (blocked()) return;
    reading = true;
    listAttempted = true;
    listKnown = false;
    syncControls();
    try { await readList(); }
    catch (error) {
      if (c.alive()) { drawRecords(); c.error(error); }
    } finally {
      reading = false;
      if (c.alive()) syncControls();
    }
  }
  async function runRead(operation, params, show, refreshList = false) {
    if (blocked()) return;
    reading = true;
    syncControls();
    try {
      const result = await c.call(operation, params);
      if (!c.alive()) return;
      show(result?.data);
      if (refreshList) {
        listKnown = false;
        await readList();
      }
    } catch (error) {
      if (c.alive()) { if (refreshList) drawRecords(); c.error(error); }
    } finally {
      reading = false;
      if (c.alive()) syncControls();
    }
  }
  async function mutate(title, message, operation, params) {
    if (!mayMutate()) return;
    mutating = true;
    syncControls();
    let reload = false;
    try {
      const result = await c.action(t(title), message, operation, params);
      if (!c.alive()) return;
      if (result === undefined) return;
      reload = true;
      showAction(operation, result.data);
    } catch (error) {
      if (!c.alive()) return;
      reload = true;
      resultBox.replaceChildren(ui.el("p", "zcode-error", t("If the request reached the vendor, its outcome may be unknown. Inspect the execution before another attempt; it is never retried automatically.")));
      c.error(error);
    } finally {
      mutating = false;
      if (c.alive()) {
        if (reload && !c.stale) await loadList();
        else syncControls();
      }
    }
  }

  const offState = c.onStateChange(() => {
    if (!c.alive()) return;
    syncControls();
    if (!listAttempted && !c.busy) loadList();
  });
  c.panelSignal.addEventListener("abort", offState, { once: true });
  drawHandoff();
  syncControls();
  loadList();
  return root;
}
