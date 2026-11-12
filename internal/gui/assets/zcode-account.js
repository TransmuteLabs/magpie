// The account sheet owns one named provider/account and one captured login.
// Its panel reads can be abandoned; a confirmed mutation cannot.
import { zcodeGiftsPanel } from "./zcode-gifts.js";
import { zcodeExecutionPanel } from "./zcode-execution.js";

async function zcodeAccountAcquire(dispose, root) {
  if (confirmationPending) return false;
  if (editing !== null || importing || importingApps) {
    if (!await cancelEdit()) return false;
    await modalDone;
  }
  const previous = zcodeAccountOwner;
  zcodeAccountOwner = null;
  previous?.dispose();
  zcodeAccountOwner = { dispose, root };
  return true;
}
function zcodeAccountRelease(dispose) {
  if (zcodeAccountOwner?.dispose === dispose) zcodeAccountOwner = null;
}

const zcodeAccountReads = new Set([
  "selection.list", "entitlement", "usage.balance", "reset.status",
  "usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.mcp", "usage.health",
  "billing.preview", "marketing.feed", "execution.list", "execution.status",
  "offpeak.availability", "offpeak.status", "highspeed.healthy",
]);
const zcodeAccountMutations = new Set([
  "selection.select", "reset.use", "reset.resolve", "billing.claim", "marketing.action",
  "execution.take", "execution.draw", "execution.continue", "execution.finish", "execution.cancel", "execution.resolve",
]);

function zcodeAccountValue(value) {
  if (value === null || value === undefined || value === "") return t("Unknown");
  if (typeof value === "boolean") return value ? t("Yes") : t("No");
  if (typeof value === "number") return Number.isFinite(value) ? String(value) : t("Unknown");
  return String(value);
}
function zcodeAccountTime(value, unit = "ms") {
  if (value === undefined || value === null || value === "") return t("Unknown");
  const date = new Date(typeof value === "number" && unit === "s" ? value * 1000 : value);
  return Number.isNaN(date.getTime()) ? t("Unknown") : date.toLocaleString(intlLang());
}
function zcodeAccountPair(box, label, value) {
  box.append(el("dt", "", t(label)), el("dd", "", zcodeAccountValue(value)));
}
function zcodeAccountSection(title) {
  const box = el("section", "zcode-section");
  box.append(el("h3", "", t(title)));
  return box;
}
function zcodeAccountNote(text, cls = "zcode-empty") { return el("div", cls, text); }
function zcodeAccountButton(label, run, cls = "text") {
  return magpie.ui.button(t(label), run, cls);
}
function zcodeAccountMutation(button) { button.dataset.zcodeMutation = ""; return button; }
function zcodeAccountRefresh(button) { button.dataset.zcodeRefresh = ""; return button; }
function zcodeAccountOutcomeUnknown(error) {
  const failure = error?.accountError;
  if (failure?.dispatched === false) return false;
  if (failure?.kind === "business" || failure?.kind === "verification_unavailable") return false;
  if (failure?.kind === "http") {
    return !Number.isInteger(failure.httpStatus) || failure.httpStatus < 400
      || failure.httpStatus === 408 || failure.httpStatus >= 500;
  }
  // A missing dispatch observation, transport loss or identity change
  // cannot establish whether the vendor accepted the mutation.
  return true;
}
function zcodeAccountTable(columns, rows) {
  const wrap = el("div", "zcode-table"), table = el("table"), head = el("thead"), headings = el("tr");
  for (const column of columns) headings.append(el("th", "", t(column[0])));
  head.append(headings);
  const body = el("tbody");
  for (const row of rows) {
    const tr = el("tr");
    for (const column of columns) tr.append(el("td", "", zcodeAccountValue(column[1](row))));
    body.append(tr);
  }
  table.append(head, body);
  wrap.append(table);
  return wrap;
}
function zcodeAccountRows(obj, fields) {
  const box = el("dl", "zcode-kv");
  for (const [key, label, time] of fields) zcodeAccountPair(box, label, time ? zcodeAccountTime(obj?.[key]) : obj?.[key]);
  return box;
}
function zcodeAccountTimeline(section, times, periods, series) {
  const arrays = series.map(([label, values]) => [label, Array.isArray(values) ? values : []]);
  const ts = Array.isArray(times) ? times : [], ps = Array.isArray(periods) ? periods : [];
  const n = Math.max(ts.length, ps.length, ...arrays.map(([, values]) => values.length));
  if (!n) return;
  const columns = [["Time", (i) => ts[i]]];
  if (ps.length) columns.push(["Period type", (i) => ps[i]]);
  for (const [label, values] of arrays) columns.push([label, (i) => values[i]]);
  section.append(zcodeAccountTable(columns, Array.from({ length: n }, (_, i) => i)));
}
function zcodeAccountObservation(o) {
  const box = el("div", "zcode-list");
  box.append(el("b", "", o?.name || t(o?.kind === "server-mcp" ? "Server MCP" : "Start balance")));
  const values = el("dl", "zcode-kv");
  for (const [key, label] of [["used", "Used"], ["limit", "Limit"], ["remaining", "Remaining"], ["unit", "Unit"]]) {
    zcodeAccountPair(values, label, o?.[key]);
  }
  zcodeAccountPair(values, "Models", Array.isArray(o?.models) ? o.models.join(", ") || t("None reported") : undefined);
  for (const [key, label] of [["userPlanId", "User plan ID"], ["planId", "Plan ID"], ["bucketId", "Bucket ID"], ["entitlementId", "Entitlement ID"]]) {
    zcodeAccountPair(values, label, o?.identity?.[key]);
  }
  for (const [key, label] of [["periodStart", "Period start"], ["periodEnd", "Period end"], ["expiresAt", "Expires at"]]) {
    zcodeAccountPair(values, label, zcodeAccountTime(o?.identity?.[key]));
  }
  zcodeAccountPair(values, "Observed at", zcodeAccountTime(o?.at));
  zcodeAccountPair(values, "Next refresh", zcodeAccountTime(o?.nextRefreshAt));
  box.append(values);
  return box;
}

function zcodeAccountReports(c) {
  const panel = el("div", "zcode-account-panel");
  const options = [
    ["usage.model", "Model usage"], ["usage.tool", "Tool usage"], ["usage.activity", "Activity"],
    ["usage.detail", "Usage details"], ["usage.mcp", "Server MCP"], ["usage.health", "System health"],
  ];
  let operation = "usage.model", usageType = "MODEL", request = 0;
  const today = new Date(), startDate = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6);
  const dateText = (d) => [d.getFullYear(), String(d.getMonth() + 1).padStart(2, "0"), String(d.getDate()).padStart(2, "0")].join("-");
  const start = el("input"), end = el("input");
  start.type = end.type = "date";
  start.value = dateText(startDate); end.value = dateText(today);
  const form = el("div", "zcode-section"), reportField = el("div", "zcode-fields"), output = el("div"), actions = el("div", "zcode-actions");
  const kind = magpie.ui.menu(options.map(([v, name]) => ({ v, name: t(name) })), operation, t("Report"), (v) => {
    operation = v; request++; load.disabled = false; output.replaceChildren(); syncFields();
  });
  const detailType = magpie.ui.menu([{ v: "MODEL", name: t("Model") }, { v: "MCP", name: t("Server MCP") }], usageType, t("Detail type"), (v) => {
    usageType = v; request++; load.disabled = false; output.replaceChildren();
  });
  const dateFields = el("div", "zcode-fields"), detailFields = el("div", "zcode-fields");
  dateFields.append(...field(t("Start date"), start), ...field(t("End date"), end));
  detailFields.append(...field(t("Detail type"), detailType));
  reportField.append(...field(t("Report"), kind));
  form.append(reportField, dateFields, detailFields);
  function syncFields() {
    dateFields.hidden = operation === "usage.mcp";
    detailFields.hidden = operation !== "usage.detail";
  }
  start.onchange = end.onchange = () => { request++; load.disabled = false; output.replaceChildren(); };
  syncFields();
  const load = zcodeAccountRefresh(zcodeAccountButton("Read report", async () => {
    if (c.busy) return;
    const op = operation, type = usageType, from = start.value, to = end.value, turn = ++request;
    if (op !== "usage.mcp" && (!from || !to || from > to)) {
      output.replaceChildren(zcodeAccountNote(t("Choose a start date no later than the end date."), "zcode-error"));
      return;
    }
    const params = op === "usage.mcp" ? {} : { startTime: from + " 00:00:00", endTime: to + " 23:59:59", ...(op === "usage.detail" ? { usageType: type } : {}) };
    load.disabled = true;
    output.replaceChildren(zcodeAccountNote(t("Loading…")));
    try {
      const result = await c.call(op, params);
      if (!c.alive() || turn !== request) return;
      output.replaceChildren(zcodeAccountReportData(op, result.data));
    } catch (error) {
      if (!c.alive() || turn !== request || error?.name === "AbortError") return;
      output.replaceChildren(zcodeAccountNote(error.message || t("Unavailable"), "zcode-error"));
      c.error(error);
    } finally { if (c.alive() && turn === request) load.disabled = false; }
  }));
  actions.append(load);
  panel.append(form, actions, output);
  return panel;
}

function zcodeAccountReportData(operation, data) {
  const box = el("div");
  if (data === null || data === undefined) {
    box.append(zcodeAccountNote(t("Report unavailable or unknown.")));
    return box;
  }
  if (operation === "usage.mcp") {
    box.append(zcodeAccountObservation(data));
    return box;
  }
  if (operation === "usage.activity") {
    const summary = zcodeAccountSection("Summary");
    summary.append(zcodeAccountRows(data.summary, [
      ["totalTokens", "Total tokens"], ["peakDailyTokens", "Peak daily tokens"], ["peakDailyTokensDate", "Peak date"],
      ["totalUsageDurationMs", "Usage duration (ms)"], ["currentStreakDays", "Current streak (days)"], ["longestStreakDays", "Longest streak (days)"],
    ]));
    const history = zcodeAccountSection("Time series");
    if (Array.isArray(data.series) && data.series.length) history.append(zcodeAccountTable([
      ["Date", (r) => r.date], ["Total tokens", (r) => r.totalTokens], ["Model calls", (r) => r.modelCallCount], ["MCP calls", (r) => r.mcpCalls],
    ], data.series));
    else history.append(zcodeAccountNote(Array.isArray(data.series) ? t("Nothing in this period") : t("Unknown")));
    box.append(summary, history);
    return box;
  }
  if (operation === "usage.detail") {
    const summary = zcodeAccountSection("Summary");
    summary.append(zcodeAccountTable([
      ["Metric", (r) => t(r[1])], ["Value", (r) => data.summary?.[r[0]]?.value], ["Trend", (r) => data.summary?.[r[0]]?.trend],
    ], [["cacheHitRate", "Cache hit rate"], ["totalCredits", "Total credits"], ["averageDailyCredits", "Average daily credits"]]));
    box.append(summary);
    for (const [key, list, title] of [["modelUsage", "modelDataList", "Model usage"], ["mcpUsage", "mcpDataList", "MCP usage"]]) {
      const group = data[key];
      if (!group) continue;
      const section = zcodeAccountSection(title);
      if (!Array.isArray(group[list])) {
        zcodeAccountTimeline(section, group.xTime, group.periodTypes, []);
        section.append(zcodeAccountNote(t("Unknown")));
      } else if (!group[list].length) {
        zcodeAccountTimeline(section, group.xTime, group.periodTypes, []);
        section.append(zcodeAccountNote(t("Nothing in this period")));
      }
      else for (const item of group[list]) {
        const entry = el("div", "zcode-list");
        const fields = key === "modelUsage" ? [
          ["modelName", "Model"], ["modelCode", "Model code"], ["sortOrder", "Sort order"], ["totalTokens", "Total tokens"], ["totalCredits", "Total credits"],
        ] : [
          ["mcpName", "MCP name"], ["mcpCode", "MCP code"], ["toolName", "Tool"], ["toolCode", "Tool code"], ["sortOrder", "Sort order"], ["totalUsageCount", "Total calls"], ["totalCredits", "Total credits"],
        ];
        entry.append(zcodeAccountRows(item, fields));
        const series = key === "modelUsage" ? [
          ["Total credits", item.totalCreditsUsage], ["Cached input credits", item.cachedInputCreditsUsage], ["Uncached input credits", item.uncachedInputCreditsUsage],
          ["Output credits", item.outputCreditsUsage], ["Tokens", item.tokensUsage], ["Total tokens", item.totalTokensUsage],
          ["Cached input tokens", item.cachedInputTokensUsage], ["Uncached input tokens", item.uncachedInputTokensUsage], ["Output tokens", item.outputTokensUsage],
        ] : [["Credits", item.creditsUsage], ["MCP calls", item.mcpCallCount], ["Tool calls", item.usageCount]];
        zcodeAccountTimeline(entry, group.xTime, group.periodTypes, series);
        section.append(entry);
      }
      box.append(section);
    }
    if (!data.modelUsage && !data.mcpUsage) box.append(zcodeAccountNote(t("Report unavailable or unknown.")));
    return box;
  }
  if (operation === "usage.health") {
    const section = zcodeAccountSection("System health");
    const x = Array.isArray(data.x_time) ? data.x_time : [], y = Array.isArray(data.xTime) ? data.xTime : [];
    const n = Math.max(x.length, y.length, data.proMaxDecodeSpeed?.length || 0, data.liteDecodeSpeed?.length || 0);
    if (n) section.append(zcodeAccountTable([
      ["Time", (i) => x[i]], ["Alternate time", (i) => y[i]], ["Max/Pro decode speed (tokens/s)", (i) => data.proMaxDecodeSpeed?.[i]], ["Lite decode speed (tokens/s)", (i) => data.liteDecodeSpeed?.[i]],
    ], Array.from({ length: n }, (_, i) => i)));
    else section.append(zcodeAccountNote(
      [data.x_time, data.xTime, data.proMaxDecodeSpeed, data.liteDecodeSpeed].some(Array.isArray) ? t("Nothing in this period") : t("Unknown"),
    ));
    box.append(section);
    return box;
  }
  const model = operation === "usage.model";
  const summary = zcodeAccountSection("Summary"), total = data.totalUsage;
  summary.append(zcodeAccountRows(data, [["granularity", "Granularity"]]));
  summary.append(zcodeAccountRows(total, model ? [["totalModelCallCount", "Model calls"], ["totalTokensUsage", "Total tokens"]] : [
    ["totalNetworkSearchCount", "Network searches"], ["totalWebReadMcpCount", "Web read calls"], ["totalZreadMcpCount", "Zread calls"], ["totalSearchMcpCount", "Search MCP calls"],
  ]));
  const summaryKey = model ? "modelSummaryList" : "toolSummaryList";
  for (const [label, list] of [["Report summary", data[summaryKey]], ["Total usage summary", total?.[summaryKey]]]) {
    if (!Array.isArray(list)) continue;
    summary.append(el("h4", "", t(label)));
    if (!list.length) { summary.append(zcodeAccountNote(t("Nothing in this period"))); continue; }
    summary.append(zcodeAccountTable(model ? [
      ["Model", (r) => r.modelName], ["Total tokens", (r) => r.totalTokens], ["Sort order", (r) => r.sortOrder],
    ] : [["Tool", (r) => r.toolName], ["Tool code", (r) => r.toolCode], ["Total calls", (r) => r.totalUsageCount], ["Sort order", (r) => r.sortOrder]], list));
  }
  box.append(summary);
  const timeline = zcodeAccountSection("Time series");
  zcodeAccountTimeline(timeline, data.x_time, null, model ? [["Model calls", data.modelCallCount], ["Tokens", data.tokensUsage]] : [
    ["Network searches", data.networkSearchCount], ["Web read calls", data.webReadMcpCount], ["Zread calls", data.zreadMcpCount],
  ]);
  if (timeline.children.length === 1) {
    const arrays = model ? [data.x_time, data.modelCallCount, data.tokensUsage] : [data.x_time, data.networkSearchCount, data.webReadMcpCount, data.zreadMcpCount];
    timeline.append(zcodeAccountNote(arrays.some(Array.isArray) ? t("Nothing in this period") : t("Unknown")));
  }
  box.append(timeline);
  const list = data[model ? "modelDataList" : "toolDataList"], bySubject = zcodeAccountSection(model ? "Models" : "Tools");
  if (!Array.isArray(list)) bySubject.append(zcodeAccountNote(t("Unknown")));
  else if (!list.length) bySubject.append(zcodeAccountNote(t("Nothing in this period")));
  else for (const item of list) {
    const entry = el("div", "zcode-list");
    entry.append(zcodeAccountRows(item, model ? [["modelName", "Model"], ["totalTokens", "Total tokens"], ["sortOrder", "Sort order"]] : [
      ["toolName", "Tool"], ["toolCode", "Tool code"], ["totalUsageCount", "Total calls"], ["sortOrder", "Sort order"],
    ]));
    zcodeAccountTimeline(entry, data.x_time, null, model ? [["Tokens", item.tokensUsage]] : [["Tool calls", item.usageCount]]);
    bySubject.append(entry);
  }
  box.append(bySubject);
  return box;
}

function zcodeAccountDetails(c, switchAccount) {
  const panel = el("div", "zcode-account-panel");
  const identity = zcodeAccountSection("Account identity");
  identity.append(zcodeAccountRows(c.context, [
    ["family", "Family"], ["userId", "User ID"], ["kind", "Kind"], ["organization", "Organization"], ["project", "Project"], ["product", "Product"],
  ]));
  const choices = zcodeAccountSection("Account selections"), entitlement = zcodeAccountSection("Subscription"), balance = zcodeAccountSection("Start balance"), reset = zcodeAccountSection("ZCode resets");
  panel.append(identity, choices, entitlement, balance, reset);

  function readInto(section, operation, render) {
    const contents = el("div");
    section.append(contents);
    contents.append(zcodeAccountNote(t("Loading…")));
    c.call(operation).then((result) => {
      if (!c.alive()) return;
      contents.replaceChildren();
      render(contents, result.data);
    }).catch((error) => {
      if (!c.alive() || error?.name === "AbortError") return;
      contents.replaceChildren(zcodeAccountNote(error.message || t("Unavailable"), "zcode-error"));
      c.error(error);
    });
  }
  readInto(choices, "selection.list", (contents, data) => {
    if (!Array.isArray(data?.selections)) { contents.append(zcodeAccountNote(t("Unknown"))); return; }
    if (!data.selections.length) { contents.append(zcodeAccountNote(t("No account selections reported."))); return; }
    for (const candidate of data.selections) {
      const row = el("div", "zcode-list"), fields = el("dl", "zcode-kv");
      const kind = candidate.kind === "individual" ? t("Personal") : candidate.kind === "team" ? t("Team") : candidate.kind === "start" ? t("Start plan") : zcodeAccountValue(candidate.kind);
      row.append(el("b", "", candidate.name ? kind + " · " + candidate.name : kind));
      for (const [key, label] of [["family", "Family"], ["userId", "User ID"], ["kind", "Kind"], ["organization", "Organization"], ["project", "Project"], ["product", "Product"]]) {
        zcodeAccountPair(fields, label, candidate[key]);
      }
      const availability = candidate.availability === "available" ? t("Available") : candidate.availability === "unavailable" ? t("Unavailable") : t("Unknown");
      zcodeAccountPair(fields, "Availability", availability);
      const pick = zcodeAccountMutation(zcodeAccountButton("Select", () => switchAccount(candidate)));
      pick.disabled = candidate.availability === "unavailable";
      row.append(fields, pick);
      contents.append(row);
    }
  });
  readInto(entitlement, "entitlement", (contents, data) => {
    const state = data?.kind === "available" ? t("Available") : data?.kind === "unavailable" ? t("Unavailable") : t("Unknown");
    const fields = el("div", "zcode-list"), details = el("dl", "zcode-kv");
    zcodeAccountPair(details, "Availability", state);
    if (data?.reason) zcodeAccountPair(details, "Reason", data.reason);
    fields.append(details);
    if (data?.subscription) fields.append(zcodeAccountRows(data.subscription, [
      ["productId", "Product ID"], ["productName", "Product"], ["status", "Status"], ["inCurrentPeriod", "Current period"],
      ["autoRenew", "Automatic renewal"], ["nextRenewTime", "Next renewal"], ["valid", "Validity"], ["billingCycle", "Billing cycle"],
      ["hasSubscription", "Has subscription"], ["memberGrantStatus", "Member grant status"], ["subscribeEndTime", "Subscription ends"], ["subscribePeriod", "Subscription period"],
    ]));
    contents.append(fields);
  });
  readInto(balance, "usage.balance", (contents, data) => {
    if (data?.kind !== "available" || !Array.isArray(data.observations)) { contents.append(zcodeAccountNote(t("Unknown"))); return; }
    if (!data.observations.length) { contents.append(zcodeAccountNote(t("No balance buckets reported."))); return; }
    for (const observation of data.observations) contents.append(zcodeAccountObservation(observation));
  });
  function readReset() {
    const old = reset.querySelector(".zcode-reset-content");
    const contents = el("div", "zcode-reset-content");
    old ? old.replaceWith(contents) : reset.append(contents);
    contents.append(zcodeAccountNote(t("Loading…")));
    c.call("reset.status").then((result) => {
      if (!c.alive() || !contents.isConnected) return;
      const r = result.data || {};
      contents.replaceChildren();
      const five = Array.isArray(r.fiveHour) ? r.fiveHour : [], week = Array.isArray(r.week) ? r.week : [];
      const fields = el("dl", "zcode-kv");
      if (r.available) {
        contents.append(el("p", "", t("ZCode resets: {five} five-hour · {week} weekly", { five: five.length, week: week.length })));
        zcodeAccountPair(fields, "Five-hour expiry", five.length ? five.map((x) => c.formatTime(x)).join(", ") : t("None reported"));
        zcodeAccountPair(fields, "Weekly expiry", week.length ? week.map((x) => c.formatTime(x)).join(", ") : t("None reported"));
      } else {
        contents.append(zcodeAccountNote(t("ZCode reset unavailable") + (r.error ? ": " + t(r.error) : "")));
      }
      if (r.pending) {
        zcodeAccountPair(fields, "Status", t(r.pending === "observed-completion" ? "Completion observed; its cause is uncertain" : "Reset outcome unconfirmed"));
        zcodeAccountPair(fields, "Reset window", r.pendingWindow);
        if (r.observedAt) zcodeAccountPair(fields, "Observed at", c.formatTime(r.observedAt));
      } else if (r.last) zcodeAccountPair(fields, "Status", t("Completion observed"));
      if (r.last) {
        zcodeAccountPair(fields, "Last reset window", r.last.window);
        zcodeAccountPair(fields, "Last reset time", c.formatTime(r.last.usedAt));
      }
      if (r.nextOpportunityAt) zcodeAccountPair(fields, "Next opportunity", c.formatTime(r.nextOpportunityAt));
      contents.append(fields);
      const actions = el("div", "zcode-actions");
      if (r.pending) {
        const resolve = zcodeAccountMutation(zcodeAccountButton("Resolve uncertainty", async () => {
          try {
            const out = await c.action(t("Resolve uncertainty"), t("The vendor may already have used this reset. Resolving uncertainty permits a future reset to be spent; check the account first."), "reset.resolve", {});
            if (out && c.alive()) readReset();
          } catch (error) { if (c.alive()) c.error(error); }
        }));
        actions.append(resolve);
      } else if (r.available) for (const [window, cards, label] of [["five-hour", five, "Use five-hour reset"], ["week", week, "Use weekly reset"]]) {
        const use = zcodeAccountMutation(zcodeAccountButton(label, async () => {
          try {
            const out = await c.action(t(label), t("Spend one reset for {who}? This cannot be undone; an accepted request may remain unconfirmed.", { who: c.source.user || c.source.account }), "reset.use", { window });
            if (out && c.alive()) readReset();
          } catch (error) { if (c.alive()) c.error(error); }
        }));
        use.disabled = !cards.length;
        actions.append(use);
      }
      contents.append(actions);
    }).catch((error) => {
      if (!c.alive() || error?.name === "AbortError" || !contents.isConnected) return;
      contents.replaceChildren(zcodeAccountNote(error.message || t("ZCode reset unavailable"), "zcode-error"));
      c.error(error);
    });
  }
  readReset();
  return panel;
}

// ZCode reset availability is independent of the legacy read-only list.
function zcodeResetRow(q) {
  const r = q.zcodeReset, row = el("div", "quota-resets zcode-resets");
  if (!r.available) {
    row.append(el("span", "", t("ZCode reset unavailable") + (r.error ? ": " + t(r.error) : "")));
  } else if (!q.resets?.count) {
    row.append(el("span", "", t("ZCode resets: {five} five-hour · {week} weekly", {
      five: r.fiveHour?.length || 0, week: r.week?.length || 0,
    })));
  }
  const separator = row.childElementCount ? " · " : "";
  if (r.pending) {
    row.append(el("span", "", separator + t(r.pending === "observed-completion"
      ? "Completion observed; its cause is uncertain" : "Reset outcome unconfirmed")));
  } else if (r.available && r.last) row.append(el("span", "", separator + t("Completion observed")));
  if (hasZCodeUser(q)) row.append(zcodeAccountEntry(q));
  return row;
}

function zcodeObservationRow(o) {
  const row = el("div", "quota-balance zcode-observation");
  const title = o.name || (o.kind === "server-mcp" ? t("Server MCP") : t("Start balance"));
  let value = t("Unavailable");
  if (o.remaining != null) value = t("{n} {unit} left", { n: o.remaining, unit: o.unit || "" });
  else if (o.used != null && o.limit != null) value = `${o.used} / ${o.limit} ${o.unit || ""}`;
  row.append(el("span", "", title), el("b", "", value));
  if (o.models?.length) row.title = o.models.join(", ");
  return row;
}

function zcodeAccountSource(q) {
  if (!hasZCodeUser(q)) return null;
  return { provider: q.provider, account: q.account || q.user, user: q.user, ...(q.from ? { from: q.from } : {}) };
}

function zcodeAccountEntry(q) {
  const button = el("button", "text zcode-account-open", t("ZCode account…"));
  button.type = "button";
  if ((q.provider || "").split("/").length > 2) {
    button.disabled = true;
    button.title = t("Open this account on its directly connected owner to use native controls.");
    button.append(document.createTextNode(" · " + button.title));
    return button;
  }
  button.onclick = async (event) => {
    event.stopPropagation();
    const source = zcodeAccountSource(q);
    if (!source) return;
    const page = view, editor = editing;
    const current = () => button.isConnected && view === page && editing === editor;
    button.disabled = true;
    try {
      if (current()) await openZCodeAccountEntry(button, source, current);
    } catch (error) { if (current()) statusErr(error); }
    finally { button.disabled = false; }
  };
  return button;
}

export function renderZCodeControl(q, kind, observation) {
  if (kind === "observation") return zcodeObservationRow(observation);
  if (kind === "entry" || ((kind === "quota" || kind === "panel") && !q.zcodeReset)) return zcodeAccountEntry(q);
  const row = zcodeResetRow(q);
  if (kind === "panel") row.classList.add("pq-resets");
  return row;
}

async function openZCodeAccountEntry(button, source, current) {
  // Cards name a login; ZCode controls prefer its exact saved slot.
  // A remote owner resolves its own account, never this computer's list.
  if (!source.provider.includes("/")) {
    const listed = await api("usage/zcode-accounts");
    if (!current()) return;
    const matches = (listed?.accounts || []).filter((a) => a.provider === source.provider
      && (a.account === source.account || a.user === source.user));
    if (matches.length === 1) {
      if (matches[0].error) { status(t(matches[0].error), "err"); return; }
      source.account = matches[0].account;
    }
    else if (matches.length > 1) {
      openProtoMenu(button, matches.map((a) => ({
        v: a.account, name: `${a.user} · ${a.account}`, literalName: true,
        note: a.error ? t(a.error) : "", title: a.error ? t(a.error) : "", disabled: !!a.error,
      })), "", (account) => {
        if (current()) openZCodeAccount({ ...source, account }).catch(statusErr);
      }, t("ZCode account…"));
      return;
    }
  }
  if (current()) await openZCodeAccount(source);
}

function zcodeAccountChanged(q, captured) {
  // A frozen action must not start a name-based read for a replacement login.
  // Keep the cached card visibly stale until the next independent quota read.
  // An unresolved selection has no card identity; never substitute accountId.
  if (!q.user) return;
  const key = usageAcctKey(q);
  quotaRefreshVersions.set(key, ++quotaRefreshVersion);
  const asOf = new Date(quotasAt || Date.now()).toISOString();
  quotas = (quotas || []).map((x) => usageAcctKey(x) === key
    && (!captured || !x.zcodeReset?.scope || x.zcodeReset.scope === captured.scope)
    ? { ...x, asOf: x.asOf || asOf, readDeferred: true } : x);
  renderQuotas();
  if (mode === "panel") renderPanelQuota();
}

async function openZCodeAccount(source, tab = "account") {
  return zcodeAccountOpen(source, tab, null);
}

async function zcodeAccountOpen(source, tab, expected) {
  const owner = Object.freeze({ provider: source?.provider || "", account: source?.account || "", user: source?.user || "", from: source?.from || "" });
  if (!owner.provider || !owner.account) { status(t("Choose a named provider and account."), "err"); return; }
  if (!["account", "reports", "gifts", "execution"].includes(tab)) tab = "account";
  const root = el("div", "editor roomy framed zcode-account"), head = el("div", "ehead");
  head.append(el("b", "", t("ZCode account")));
  const title = el("div", "zcode-account-owner", t("Account: {provider} / {account}", { provider: owner.provider, account: owner.account }));
  if (owner.user) title.append(document.createTextNode(" · " + owner.user));
  if (owner.from) title.append(document.createTextNode(" · " + owner.from));
  const body = el("div", "ebody"), strip = el("div"), note = el("div", "zcode-error"), panel = el("div"), bar = el("div", "bar");
  note.setAttribute("role", "status"); note.hidden = true;
  body.append(title, strip, note, panel);
  const refresh = zcodeAccountRefresh(zcodeAccountButton("Refresh account", () => {
    if (!busy) zcodeAccountOpen(owner, tab, null);
  }));
  const close = zcodeAccountButton("Close", () => { if (!confirmationPending) closeModal(); });
  bar.append(refresh, el("span", "grow"), close);
  root.append(head, body, bar);
  let disposed = false, attached = false, context = null, accountSelector, stale = false, busy = false;
  let panelController = null, panelNotify = null, token = null, panelAbort = null, contextAbort = null, observer;
  const locked = new Map();
  const modal = $("#modal");
  let down = false, up = false;
  function syncLocks() {
    for (const [button, was] of locked) {
      if (!button.isConnected || !busy && !(stale && button.hasAttribute("data-zcode-mutation"))) {
        if (button.isConnected) button.disabled = was;
        locked.delete(button);
      }
    }
    for (const button of root.querySelectorAll("[data-zcode-mutation], [data-zcode-refresh]")) {
      const block = busy || stale && button.hasAttribute("data-zcode-mutation");
      if (block) {
        if (!locked.has(button)) locked.set(button, button.disabled);
        button.disabled = true;
      }
    }
  }
  function notifyState() { syncLocks(); panelNotify?.(); }
  function sheetAlive() { return !disposed && (!attached || root.isConnected); }
  function isStaleError(error) {
    const kind = error?.accountError?.kind || error?.kind || error?.code;
    return ["account_changed", "account_scope_changed", "account_generation_changed", "scope_changed"].includes(kind);
  }
  function inlineError(error) {
    if (!sheetAlive() || error?.name === "AbortError") return;
    const message = error?.message || String(error || t("Unknown"));
    const lines = [message];
    const staleHint = t("This account changed. Refresh the account before making changes.");
    if (stale && isStaleError(error) && message !== staleHint) lines.push(staleHint);
    for (const [key, label] of [["nextTakeAt", "Next ticket availability"], ["nextDrawAt", "Next Highspeed draw"]]) {
      const value = error?.accountError?.[key];
      if (typeof value === "number" && Number.isFinite(value) && value > 0) lines.push(t(label) + ": " + zcodeAccountTime(value));
    }
    note.textContent = lines.join("\n");
    note.hidden = false;
  }
  function staleError(error) {
    if (!isStaleError(error)) return false;
    stale = true;
    if (sheetAlive()) {
      note.textContent = t("This account changed. Refresh the account before making changes.");
      note.hidden = false;
      notifyState();
    }
    return true;
  }
  function abortPanel() { panelAbort?.abort(); panelAbort = null; token = null; panelController = null; panelNotify = null; }
  function dispose() {
    if (disposed) return;
    disposed = true;
    abortPanel();
    contextAbort?.abort();
    observer?.disconnect();
    modal.removeEventListener("pointerdown", onDown, true);
    modal.removeEventListener("pointerup", onUp, true);
    modal.removeEventListener("click", onBackdrop, true);
    document.removeEventListener("keydown", onKey, true);
    zcodeAccountRelease(dispose);
  }
  function onDown(event) { down = event.target === modal; up = false; }
  function onUp(event) { up = down && event.target === modal; }
  function onBackdrop(event) {
    if (!confirmationPending && up && event.target === modal && sheetAlive()) {
      event.stopImmediatePropagation(); closeModal();
    }
  }
  function onKey(event) {
    if (!confirmationPending && !protoMenu && event.key === "Escape" && sheetAlive()) {
      event.preventDefault(); event.stopImmediatePropagation(); closeModal();
    }
  }
  function matchingContext(result, captured, operation) {
    if (operation === "selection.select") return;
    if (!result?.context || result.context.scope !== captured.scope || result.context.generation !== captured.generation) {
      const error = new Error(t("This account changed. Refresh the account before making changes."));
      error.accountError = { kind: "account_scope_changed" };
      throw error;
    }
  }
  async function changed(outcome, captured) {
    try { await zcodeAccountChanged(owner, captured); }
    catch (error) {
      inlineError(error);
      const failure = t("Account refresh failed: {error}", { error: error.message || String(error) });
      status(outcome ? outcome + " " + failure : failure, "err");
    }
  }
  function makePanelController(current, signal) {
    const alive = () => sheetAlive() && token === current;
    const listeners = new Set();
    panelNotify = () => {
      for (const listener of listeners) {
        try { listener(); } catch (error) { console.error(error); }
      }
    };
    const c = {
      source: owner,
      get context() { return context; },
      get accountSelector() { return accountSelector; },
      get panelSignal() { return signal; },
      get busy() { return busy; },
      get stale() { return stale; },
      alive,
      canMutate: () => alive() && !busy && !stale && !!context,
      onStateChange(listener) { listeners.add(listener); return () => listeners.delete(listener); },
      changed,
      outcomeUnknown: zcodeAccountOutcomeUnknown,
      error(error) { if (alive()) inlineError(error); },
      formatTime: zcodeAccountTime,
      async call(operation, params = {}) {
        if (!zcodeAccountReads.has(operation)) throw new Error(t("This operation is not a read."));
        if (!alive() || !context) throw new DOMException("Aborted", "AbortError");
        const captured = { ...context }, data = JSON.parse(JSON.stringify(params));
        try {
          const result = await api("usage/zcode-account", { provider: owner.provider, account: owner.account, operation, params: data, scope: captured.scope, generation: captured.generation }, undefined, signal);
          matchingContext(result, captured, operation);
          return result;
        } catch (error) { staleError(error); throw error; }
      },
      async action(title, message, operation, params = {}, options = {}) {
        if (!zcodeAccountMutations.has(operation)) throw new Error(t("This operation is not a change."));
        if (!alive() || busy || stale || !context) return undefined;
        // Snapshot before opening the independent confirmation dialog.
        const captured = { ...context }, frozenParams = JSON.parse(JSON.stringify(params));
        const followUp = options?.followUp ? JSON.parse(JSON.stringify(options.followUp)) : null;
        const followWhen = options?.followUp?.when;
        const beforeDispatch = typeof options === "function" ? options : options?.beforeDispatch;
        if (followUp && (operation !== "billing.claim" || followUp.operation !== "marketing.action" || followUp.params?.action_type !== "confirm")) {
          throw new Error(t("This operation is not a change."));
        }
        const who = t("Provider: {provider}; account: {account}; family: {family}; kind: {kind}; user ID: {user}", {
          provider: owner.provider, account: owner.account, family: zcodeAccountValue(captured.family), kind: zcodeAccountValue(captured.kind), user: zcodeAccountValue(captured.userId),
        });
        const statusWho = t("Account: {provider} / {account}", { provider: owner.provider, account: owner.account });
        busy = true; notifyState();
        let dispatched = false, outcome = "";
        try {
          if (!await confirmAction(title, message + "\n\n" + who, title)) return undefined;
          if (!sheetAlive()) return undefined;
          if (beforeDispatch && await beforeDispatch() === false) return undefined;
          dispatched = true;
          const result = await api("usage/zcode-account", { provider: owner.provider, account: owner.account, operation, params: frozenParams, scope: captured.scope, generation: captured.generation });
          matchingContext(result, captured, operation);
          const claimUnknown = operation === "billing.claim" && result.data?.plan?.plan_id !== frozenParams.plan_id;
          if (claimUnknown) {
            outcome = statusWho + " — " + t("The claim outcome is unknown; inspect the account before retrying.");
            status(outcome, "err");
          } else if (operation === "reset.use" && result.data?.pending) {
            outcome = statusWho + " — " + t("Reset outcome unconfirmed");
            status(outcome);
          } else {
            outcome = statusWho + " — " + t("{action} completed for {account}", { action: title, account: owner.account });
            status(outcome, "ok");
          }
          if (followUp) {
            try {
              if (claimUnknown || followWhen && !followWhen(result)) throw new Error(t("The claim result did not identify the requested plan; its delivery was not reported."));
              const report = await api("usage/zcode-account", { provider: owner.provider, account: owner.account, operation: followUp.operation, params: followUp.params, scope: captured.scope, generation: captured.generation });
              matchingContext(report, captured, followUp.operation);
            } catch (error) {
              staleError(error);
              result.followUpError = error;
              outcome = statusWho + " — " + (claimUnknown ? t("The claim outcome is unknown; inspect the account before retrying.")
                : t("The action completed, but its delivery report failed or is unknown. Check before retrying."));
              status(outcome, "err");
              inlineError(error);
            }
          }
          return result;
        } catch (error) {
          staleError(error);
          if (dispatched && zcodeAccountOutcomeUnknown(error)) {
            outcome = statusWho + " — " + t("{action}: outcome unknown for {account}; inspect or refresh before retrying.", { action: title, account: owner.account });
            status(outcome, "err");
          } else {
            outcome = statusWho + " — " + (error.message || String(error));
            status(outcome, "err");
          }
          throw error;
        } finally {
          busy = false; notifyState();
          if (dispatched) void changed(outcome, captured);
        }
      },
    };
    return c;
  }
  async function switchAccount(candidate) {
    if (!panelController?.canMutate()) return;
    const selection = {}, identityFields = ["kind", "family", "userId", "organization", "project", "product"];
    for (const key of identityFields) if (candidate[key] !== undefined) selection[key] = candidate[key];
    const description = [
      t("Switch to this selection? The current account is not overwritten."),
      ...[["Family", "family"], ["User ID", "userId"], ["Kind", "kind"], ["Organization", "organization"], ["Project", "project"], ["Product", "product"]].map(([label, key]) => t(label) + ": " + zcodeAccountValue(selection[key])),
    ].join("\n");
    try {
      const result = await panelController.action(t("Select account"), description, "selection.select", { selection });
      if (!result) return;
      if (!result.data?.account || !result.context?.scope || !result.context?.generation ||
          identityFields.some((key) => String(selection[key] ?? "") !== String(result.context[key] ?? ""))) {
        const error = new Error(t("The selected account could not be identified. Refresh the accounts before trying again."));
        statusErr(error); inlineError(error); return;
      }
      if (sheetAlive()) await zcodeAccountOpen({ ...owner, account: result.data.account, user: result.data.user || "" }, tab, result.context);
    } catch (error) { inlineError(error); }
  }
  function showPanel(next) {
    abortPanel();
    tab = next;
    strip.replaceChildren(segs([["account", t("Account")], ["reports", t("Reports")], ["gifts", t("Gifts")], ["execution", t("Execution")]], tab, showPanel));
    panel.replaceChildren();
    if (!context) return;
    panelAbort = new AbortController();
    token = {};
    panelController = makePanelController(token, panelAbort.signal);
    const element = tab === "account" ? zcodeAccountDetails(panelController, switchAccount)
      : tab === "reports" ? zcodeAccountReports(panelController)
        : tab === "gifts" ? zcodeGiftsPanel(panelController) : zcodeExecutionPanel(panelController);
    panel.append(element);
    syncLocks();
  }
  if (!await zcodeAccountAcquire(dispose, root)) return;
  modal.addEventListener("pointerdown", onDown, true);
  modal.addEventListener("pointerup", onUp, true);
  modal.addEventListener("click", onBackdrop, true);
  document.addEventListener("keydown", onKey, true);
  openModal(root);
  attached = true;
  observer = new MutationObserver(() => {
    if (attached && !root.isConnected) dispose();
    else syncLocks();
  });
  observer.observe(modal.firstElementChild, { childList: true });
  observer.observe(root, { childList: true, subtree: true });
  showPanel(tab);
  panel.replaceChildren(zcodeAccountNote(t("Loading…")));
  const initial = new AbortController();
  contextAbort = initial;
  try {
    const params = { provider: owner.provider, account: owner.account, operation: "context" };
    if (expected?.scope && expected?.generation) Object.assign(params, { scope: expected.scope, generation: expected.generation });
    const result = await api("usage/zcode-account", params, undefined, initial.signal);
    if (!sheetAlive() || initial.signal.aborted) return;
    if (!result?.context?.scope || !result.context.generation) throw new Error(t("The account context is unavailable."));
    if (expected) matchingContext(result, expected, "context");
    context = Object.freeze({ ...result.context });
    accountSelector = typeof result.data?.accountSelector === "string" && result.data.accountSelector ? result.data.accountSelector : undefined;
    contextAbort = null;
    note.hidden = true;
    showPanel(tab);
  } catch (error) {
    if (!sheetAlive() || error?.name === "AbortError") return;
    staleError(error);
    inlineError(error);
    panel.replaceChildren(zcodeAccountNote(t("The account context is unavailable."), "zcode-error"));
  }
}
