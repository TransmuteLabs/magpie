// Billing and marketing use the account sheet's captured owner and generation.
// Campaign markup and visual assets are never interpreted or loaded here.
export function zcodeGiftsPanel(c) {
  const root = el("div", "zcode-gifts");
  const billing = el("section", "zcode-section");
  const marketing = el("section", "zcode-section");
  root.append(billing, marketing);

  let preview = null, previewError = null, previewLoading = false;
  let feed = null, feedError = null, feedLoading = false;
  let previewAttempted = false, feedAttempted = false;
  let claimResult = null, claimNotice = null, feedNotice = null, working = false;

  const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
  const messageOf = (error) => {
    if (error?.message === "\u0000blocked") return t("The browser blocked the campaign link.");
    return typeof error?.message === "string" && error.message ? error.message : t("Unknown");
  };
  const valueOf = (value) => {
    if (value === null || value === undefined) return t("Unknown");
    if (value === "") return t("(empty)");
    if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") return String(value);
    return t("Unknown");
  };
  const timeOf = (value, unit = "s", effective = false) => {
    if (typeof value !== "number" || !Number.isFinite(value)) return t("Unknown");
    if (value === 0) return effective ? t("Immediately (0)") : "0";
    return c.formatTime(value, unit);
  };
  const listOf = (value) => {
    if (!Array.isArray(value) || !value.every((item) => typeof item === "string")) return t("Unknown");
    return value.length ? value.join(", ") : t("None");
  };
  const row = (list, label, value) => list.append(el("dt", "", label), el("dd", "", value));
  const canAct = () => c.alive() && c.canMutate() && !working && !previewLoading && !feedLoading;

  function control(label, handler, mutation = false) {
    const b = magpie.ui.button(label, handler);
    b.disabled = !canAct();
    if (mutation) b.dataset.zcodeMutation = "";
    return b;
  }

  function refreshControl(handler) {
    const b = control(t("Refresh"), handler);
    b.dataset.zcodeRefresh = "";
    return b;
  }

  function notice(box, info) {
    if (!info) return;
    box.append(el("p", info.error ? "zcode-error" : "zcode-empty", info.text));
  }

  // These fields are the preview/claim projection; zero and absent values
  // remain distinct, including the effective_at=0 immediate sentinel.
  function entitlements(parent, entries, claimed = false) {
    const title = el("h4", "", t("Gift entitlements"));
    parent.append(title);
    if (!Array.isArray(entries)) {
      parent.append(el("p", "zcode-empty", t("Unknown")));
      return;
    }
    if (!entries.length) {
      parent.append(el("p", "zcode-empty", t("No gift entitlements were returned.")));
      return;
    }
    const list = el("div", "zcode-list");
    for (const entry of entries) {
      const e = isObject(entry) ? entry : {};
      const item = el("div");
      const values = el("dl", "zcode-kv");
      row(values, t("Gift entitlement ID"), valueOf(e.entitlement_id));
      row(values, t("Name"), valueOf(e.show_name));
      if (!claimed) {
        row(values, t("Gift meter"), valueOf(e.meter));
        row(values, t("Gift unit type"), valueOf(e.unit_type));
        row(values, t("Gift grant units"), valueOf(e.grant_units));
        row(values, t("Gift period"), valueOf(e.period));
        row(values, t("Gift capabilities"), listOf(e.capabilities));
        row(values, t("Gift priority"), valueOf(e.priority));
      }
      row(values, t("Gift effective time"), timeOf(e.effective_at, "s", true));
      item.append(values);
      list.append(item);
    }
    parent.append(list);
  }

  function planDetails(plan, claimed = false) {
    const item = el("div");
    const values = el("dl", "zcode-kv");
    const p = isObject(plan) ? plan : {};
    row(values, t("Gift plan ID"), valueOf(p.plan_id));
    if (claimed) {
      row(values, t("Gift user plan ID"), valueOf(p.user_plan_id));
      row(values, t("Status"), valueOf(p.status));
      row(values, t("Gift starts"), timeOf(p.starts_at));
      row(values, t("Gift ends"), timeOf(p.ends_at));
    } else {
      row(values, t("Name"), valueOf(p.name));
      row(values, t("Description"), valueOf(p.description));
      row(values, t("Gift priority"), valueOf(p.priority));
      // Campaign visuals can carry these plan times even though preview does not.
      if (Object.hasOwn(p, "starts_at")) row(values, t("Gift starts"), timeOf(p.starts_at));
      if (Object.hasOwn(p, "ends_at")) row(values, t("Gift ends"), timeOf(p.ends_at));
    }
    item.append(values);
    entitlements(item, p.entitlements, claimed);
    return item;
  }

  function claimMatches(result, planID) {
    return isObject(result?.data) && isObject(result.data.plan) && result.data.plan.plan_id === planID;
  }

  function reportParams(candidate, actionType) {
    return { scope: candidate.scope, campaign_id: candidate.delivery.campaign_id, action_type: actionType, locale: candidate.language };
  }

  function claimFailure(error) {
    if (error?.accountError?.kind === "verification_unavailable") {
      return t("ZCode claim verification is unavailable. Use the ZCode app to claim this plan.") + " " + messageOf(error);
    }
    if (c.outcomeUnknown(error)) {
      return t("The claim outcome is unknown. Check the account before trying again; magpie will not retry it.") + " " + messageOf(error);
    }
    return t("The claim failed.") + " " + messageOf(error);
  }

  function removeCandidate(candidate) {
    if (feed && candidate) feed.deliveries = feed.deliveries.filter((item) => item !== candidate);
  }

  function completeCandidate(candidate) {
    if (!feed || !candidate) return;
    const success = candidate.delivery.resource_position === "banner" && !candidate.result
      ? candidate.delivery.banner?.success_popup : null;
    if (isObject(success)) candidate.result = true;
    else removeCandidate(candidate);
  }

  async function runAction(work) {
    if (!canAct()) return;
    working = true;
    render();
    try { await work(); }
    finally {
      working = false;
      if (c.alive()) render();
    }
  }

  function claim(planID, candidate = null) {
    // Capture both the plan and campaign identity before opening confirmation.
    if (typeof planID !== "string" || !planID.trim()) return;
    const capturedPlan = planID;
    const report = candidate ? reportParams(candidate, "confirm") : null;
    runAction(async () => {
      try {
        const options = report ? { followUp: {
          operation: "marketing.action", params: report,
          when: (result) => claimMatches(result, capturedPlan),
        } } : undefined;
        const result = await c.action(t("Claim gift plan?"),
          t("Claim gift plan {plan}? This may change your account's entitlements.", { plan: capturedPlan }),
          "billing.claim", { plan_id: capturedPlan }, options);
        if (!result || !c.alive()) return;
        claimResult = isObject(result.data) ? result.data : null;
        if (!claimMatches(result, capturedPlan)) {
          claimNotice = { error: true, text: t("The claim returned a different or missing plan. Its outcome is unknown; no campaign confirmation was sent.") };
          if (candidate) feedNotice = claimNotice;
          return;
        }
        claimNotice = { error: false, text: t("ZCode returned this claim result.") };
        if (candidate) {
          completeCandidate(candidate);
          feedNotice = result.followUpError
            ? { error: true, text: t("The action succeeded, but its campaign report failed or is unknown. Do not repeat it just to report it.") + " " + messageOf(result.followUpError) }
            : { error: false, text: t("The campaign action was reported.") };
        }
      } catch (error) {
        if (!c.alive()) return;
        claimNotice = { error: true, text: claimFailure(error) };
        if (candidate) feedNotice = claimNotice;
      }
    });
  }

  async function readPreview() {
    if (!c.alive() || working || c.busy || previewLoading || c.stale) return;
    previewAttempted = true;
    previewLoading = true;
    previewError = null;
    render();
    try {
      const result = await c.call("billing.preview");
      if (!c.alive()) return;
      if (!isObject(result?.data) || !Array.isArray(result.data.plans)) throw new Error(t("The gift preview is unavailable or incomplete."));
      preview = result.data;
    } catch (error) {
      if (c.alive()) { previewError = error; c.error(error); }
    } finally {
      if (c.alive()) { previewLoading = false; render(); }
    }
  }

  async function readFeed() {
    if (!c.alive() || working || c.busy || feedLoading || c.stale) return;
    const wireLocale = locale === "zh" || locale === "zh-TW" ? "zh-CN" : "en-US";
    feedAttempted = true;
    feedLoading = true;
    feedError = null;
    render();
    try {
      const result = await c.call("marketing.feed", { locale: wireLocale });
      if (!c.alive()) return;
      const data = result?.data;
      if (!isObject(data) || !Array.isArray(data.deliveries) || typeof data.scope !== "string" || !data.scope ||
          !["zh-CN", "en-US"].includes(data.language) || !Number.isSafeInteger(data.rejectedCount) || data.rejectedCount < 0) {
        throw new Error(t("The campaign feed is unavailable or incomplete."));
      }
      // Scope and the returned language travel with each candidate, even if a
      // later refresh or a language change yields a different snapshot.
      feed = { serverTime: data.serverTime, rejectedCount: data.rejectedCount,
        deliveries: data.deliveries.map((delivery) => ({ delivery, scope: data.scope, language: data.language, result: false })) };
      feedNotice = null;
    } catch (error) {
      if (c.alive()) { feedError = error; c.error(error); }
    } finally {
      if (c.alive()) { feedLoading = false; render(); }
    }
  }

  function renderBilling() {
    const heading = el("div", "zcode-actions");
    heading.append(el("h3", "", t("ZCode gift plans")), refreshControl(readPreview));
    billing.replaceChildren(heading);
    if (previewLoading) billing.append(el("p", "zcode-empty", t("Loading…")));
    if (previewError) {
      billing.append(el("p", "zcode-error", t("Gift preview failed: {error}", { error: messageOf(previewError) })));
      if (preview) billing.append(el("p", "zcode-empty", t("Showing the previous reading; the latest refresh failed.")));
    }
    if (preview) {
      const meta = el("dl", "zcode-kv");
      row(meta, t("Gift server time"), timeOf(preview.server_time));
      billing.append(meta);
      if (!preview.plans.length) billing.append(el("p", "zcode-empty", t("No gift plans were returned.")));
      else {
        const list = el("div", "zcode-list");
        for (const plan of preview.plans) {
          const p = isObject(plan) ? plan : {};
          const item = el("article");
          item.append(planDetails(p));
          if (typeof p.plan_id === "string" && p.plan_id.trim()) {
            const actions = el("div", "zcode-actions");
            actions.append(control(t("Claim gift"), () => claim(p.plan_id), true));
            item.append(actions);
          }
          list.append(item);
        }
        billing.append(list);
      }
    }
    notice(billing, claimNotice);
    if (claimResult) {
      const result = el("div", "zcode-list");
      result.append(el("h4", "", t("Gift claim result")));
      const meta = el("dl", "zcode-kv");
      row(meta, t("Gift server time"), timeOf(claimResult.server_time));
      result.append(meta, planDetails(claimResult.plan, true));
      billing.append(result);
    }
  }

  function validURL(value) {
    if (typeof value !== "string" || value.length > 4096) return null;
    try {
      const url = new URL(value);
      return ["http:", "https:"].includes(url.protocol) && !url.username && !url.password ? url.href : null;
    } catch { return null; }
  }

  function dismiss(candidate) {
    if (candidate.result) {
      if (!canAct()) return;
      removeCandidate(candidate);
      render();
      return;
    }
    const report = reportParams(candidate, "cancel");
    runAction(async () => {
      let dispatched = false;
      try {
        const result = await c.action(t("Dismiss ZCode offer?"),
          t("Dismiss campaign {campaign} and report its cancellation?", { campaign: report.campaign_id }),
          "marketing.action", report, { beforeDispatch: () => { dispatched = true; return true; } });
        if (!result || !c.alive()) return;
        removeCandidate(candidate);
        feedNotice = { error: false, text: t("The campaign was dismissed.") };
      } catch (error) {
        if (!c.alive()) return;
        if (dispatched) removeCandidate(candidate);
        feedNotice = { error: true, text: t("The dismissal report failed or is unknown. It will not be retried automatically.") + " " + messageOf(error) };
      }
    });
  }

  function perform(candidate, action, button) {
    const report = reportParams(candidate, "confirm");
    if (action.type === "claim_zcode_plan") {
      claim(action.args?.plan_id, candidate);
      return;
    }
    const isCopy = action.type === "copy_text";
    const text = isCopy && typeof action.args?.text === "string" && action.args.text.trim() && action.args.text.length <= 20000
      ? action.args.text : null;
    const url = action.type === "open_url" ? validURL(action.args?.url) : null;
    if (!text && !url) {
      feedNotice = { error: true, text: t("This campaign action is unavailable because its details are invalid.") };
      render();
      return;
    }
    runAction(async () => {
      let attempted = false, completed = false, localError = null;
      const beforeDispatch = async () => {
        attempted = true;
        try {
          if (isCopy) {
            if (!await copy(text, t("Campaign text"), button, t("Copied"))) return false;
          } else {
            if (web) openSignInPage(url);
            else await api("open", { url });
          }
          completed = true;
          return true;
        } catch (error) { localError = error; return false; }
      };
      try {
        const result = await c.action(isCopy ? t("Copy campaign text?") : t("Open campaign link?"),
          isCopy ? t("Copy the displayed campaign text and report success?")
            : t("Open {url} and report success?", { url }),
          "marketing.action", report, { beforeDispatch });
        if (!c.alive()) return;
        if (!result) {
          if (attempted) feedNotice = { error: true, text: t("The action did not complete; no campaign confirmation was sent.") + (localError ? " " + messageOf(localError) : "") };
          return;
        }
        if (!isCopy) completeCandidate(candidate);
        feedNotice = { error: false, text: t("The campaign action was reported.") };
      } catch (error) {
        if (!c.alive()) return;
        if (completed && !isCopy) completeCandidate(candidate);
        feedNotice = { error: true, text: (completed
          ? t("The action succeeded, but its campaign report failed or is unknown. Do not repeat it just to report it.")
          : t("The action did not complete; no campaign confirmation was sent.")) + " " + messageOf(error) };
      }
    });
  }

  function campaignVisual(parent, visual) {
    if (!isObject(visual)) return;
    const values = el("dl", "zcode-kv");
    row(values, t("ZCode visual type"), valueOf(visual.type));
    parent.append(values, el("p", "zcode-empty", t("Campaign visuals are not loaded in magpie.")));
    const plan = visual.args?.zcode_plan;
    if (isObject(plan)) parent.append(planDetails(plan));
  }

  function campaign(candidate) {
    const d = candidate.delivery;
    if (!isObject(d)) return el("p", "zcode-error", t("This campaign is unavailable."));
    const banner = d.resource_position === "banner";
    const content = candidate.result ? d.banner?.success_popup : banner ? d.banner : d.popup;
    if (!isObject(content) || typeof d.campaign_id !== "string" || !d.campaign_id) return el("p", "zcode-error", t("This campaign is unavailable."));
    const item = el("article");
    item.append(el("h4", "", candidate.result ? t("ZCode campaign result") : banner ? t("ZCode banner") : t("ZCode popup")));
    const meta = el("dl", "zcode-kv");
    row(meta, t("ZCode campaign ID"), d.campaign_id);
    row(meta, t("Gift priority"), valueOf(d.priority));
    row(meta, t("Language"), candidate.language);
    item.append(meta);
    if (isObject(content.title)) item.append(el("p", "", valueOf(content.title.content)));
    if (isObject(content.description)) item.append(el("p", "", valueOf(content.description.content)));
    campaignVisual(item, candidate.result || !banner ? content.hero : content.background);
    const actions = el("div", "zcode-actions");
    let closeLabel = "";
    const buttons = Array.isArray(content.buttons) ? content.buttons : [];
    for (const data of buttons) {
      const action = isObject(data?.action) ? data.action : {};
      const label = typeof data?.text?.content === "string" && data.text.content.trim() ? data.text.content : "";
      if (action.type === "close") { if (!closeLabel) closeLabel = label; continue; }
      if (action.type === "navigate") {
        const destination = el("dl", "zcode-kv");
        row(destination, t("ZCode destination"), valueOf(action.args?.page));
        if (action.args?.section !== undefined) row(destination, t("ZCode destination section"), valueOf(action.args.section));
        if (action.args?.provider_id !== undefined) row(destination, t("ZCode destination provider"), valueOf(action.args.provider_id));
        if (action.args?.plugin_id !== undefined) row(destination, t("ZCode destination plugin"), valueOf(action.args.plugin_id));
        item.append(destination, el("p", "zcode-empty", t("This ZCode destination is unavailable in magpie: no documented route can open it.")));
        const disabled = el("button", "text", label || t("Open"));
        disabled.type = "button";
        disabled.disabled = true;
        actions.append(disabled);
        continue;
      }
      if (!["claim_zcode_plan", "copy_text", "open_url"].includes(action.type)) {
        item.append(el("p", "zcode-error", t("This campaign action is unavailable.")));
        continue;
      }
      if (action.type === "claim_zcode_plan") {
        const detail = el("dl", "zcode-kv");
        row(detail, t("Gift plan ID"), valueOf(action.args?.plan_id));
        item.append(detail);
      } else if (action.type === "copy_text") {
        const detail = el("dl", "zcode-kv");
        row(detail, t("Campaign text"), valueOf(action.args?.text));
        item.append(detail);
      } else {
        const detail = el("dl", "zcode-kv");
        row(detail, t("Campaign link"), valueOf(action.args?.url));
        item.append(detail);
      }
      const buttonLabel = label || t(action.type === "claim_zcode_plan" ? "Claim gift" : action.type === "copy_text" ? "Copy" : "Open");
      const invalid = action.type === "claim_zcode_plan" && (typeof action.args?.plan_id !== "string" || !action.args.plan_id.trim()) ||
          action.type === "copy_text" && (typeof action.args?.text !== "string" || !action.args.text.trim() || action.args.text.length > 20000) ||
          action.type === "open_url" && !validURL(action.args?.url);
      if (invalid) {
        const disabled = el("button", "text", buttonLabel);
        disabled.type = "button";
        disabled.disabled = true;
        item.append(el("p", "zcode-error", t("This campaign action is unavailable because its details are invalid.")));
        actions.append(disabled);
        continue;
      }
      let button;
      button = control(buttonLabel, () => perform(candidate, action, button), true);
      actions.append(button);
    }
    actions.append(control(closeLabel || (candidate.result ? t("Close") : t("Dismiss offer")), () => dismiss(candidate), !candidate.result));
    item.append(actions);
    return item;
  }

  function renderMarketing() {
    const heading = el("div", "zcode-actions");
    heading.append(el("h3", "", t("ZCode offers")), refreshControl(readFeed));
    marketing.replaceChildren(heading);
    if (feedLoading) marketing.append(el("p", "zcode-empty", t("Loading…")));
    if (feedError) {
      marketing.append(el("p", "zcode-error", t("Campaign feed failed: {error}", { error: messageOf(feedError) })));
      if (feed) marketing.append(el("p", "zcode-empty", t("Showing the previous reading; the latest refresh failed.")));
    }
    notice(marketing, feedNotice);
    if (!feed) return;
    const meta = el("dl", "zcode-kv");
    row(meta, t("Campaign server time"), timeOf(feed.serverTime, "ms"));
    marketing.append(meta);
    if (feed.rejectedCount) marketing.append(el("p", "zcode-error", t("{n} campaign deliveries were rejected; this feed is partial.", { n: feed.rejectedCount })));
    if (!feed.deliveries.length) marketing.append(el("p", "zcode-empty", t("No valid ZCode offers were returned.")));
    else {
      const list = el("div", "zcode-list");
      for (const candidate of feed.deliveries) list.append(campaign(candidate));
      marketing.append(list);
    }
  }

  function render() {
    if (!c.alive()) return;
    renderBilling();
    renderMarketing();
  }

  const unsubscribe = c.onStateChange(() => {
    render();
    if (!c.alive() || c.busy || c.stale || working) return;
    if (!previewAttempted) readPreview();
    if (!feedAttempted) readFeed();
  });
  c.panelSignal.addEventListener("abort", unsubscribe, { once: true });
  render();
  readPreview();
  readFeed();
  return root;
}
