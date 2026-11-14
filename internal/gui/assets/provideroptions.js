window.paintProviderOptions = function(box, options, values, confirmed) {
  box.replaceChildren();
  for (const option of options) {
    const row = el("div", "plugin-option");
    row.dataset.key = option.key;
    const text = el("div", "plugin-option-text");
    const label = optionWords(option, "label"), about = optionWords(option, "description");
    const needsConfirmation = !!option.confirm || Object.keys(option.confirms || {}).length > 0;
    const ask = optionWords(option, "confirm") || Object.values(option.confirms || {}).find(value => value.trim()) || t("Turn on {name}?", { name: label });
    text.append(el("div", "n", label));
    if (about) text.append(el("div", "hint", about));
    const now = () => Object.hasOwn(values, option.key) ? values[option.key] : !!option.on;
    const draw = () => {
      const control = segs([["off", t("Off")], ["on", t("On")]], now() ? "on" : "off", async value => {
        const on = value === "on";
        if (on && !now() && needsConfirmation && !(await confirmAction(t("Turn on {name}?", { name: label }), ask, t("Turn on")))) {
          control.replaceWith(draw());
          return;
        }
        if (on && needsConfirmation) confirmed.add(option.key);
        else confirmed.delete(option.key);
        if (on === !!option.on) delete values[option.key];
        else values[option.key] = on;
      });
      return control;
    };
    row.append(text, draw());
    box.append(row);
  }
};

function sameProviderOptionsOwner(first, second) {
  return first && second && ["module", "provider", "packageRevision", "configVersion"].every(key => first[key] === second[key]);
}

function providerOptionsChanged(provider) {
  return provider.extensionOptionsError || t("Provider options changed; reopen this editor before saving.");
}

window.saveProviderOptionDraft = async function(editingDraft, subscription, provider) {
  if (editingDraft.extensionOwner || provider.extensionOptions?.options?.length || provider.extensionOptionsError || provider.extensionOptionsUnknown) {
    await saveExtensionProviderOptions(editingDraft, provider);
    if (draft !== editingDraft) throw new Error(providerOptionsChanged(provider));
  }
  const providerID = subscription?.plugin?.pid;
  if (!providerID) return;
  for (const [key, on] of Object.entries(editingDraft.pluginOptions || {})) {
    prefs = await writingPrefs(api("settings/plugin-option", { provider: providerID, key, on }));
  }
};

window.paintExtensionProviderOptions = function(box, editingDraft, provider) {
  const owner = editingDraft.extensionOwner;
  if (editingDraft.extensionOptionsUnknown || !sameProviderOptionsOwner(owner, provider.extensionOptions)) {
    box.replaceChildren(el("div", "hint", providerOptionsChanged(provider)));
    return;
  }
  paintProviderOptions(box, owner.options, editingDraft.extensionOptions ??= {}, editingDraft.extensionConfirms ??= new Set());
};

window.saveExtensionProviderOptions = async function(editingDraft, provider) {
  if (editingDraft.extensionOptionsUnknown || provider.extensionOptionsUnknown || provider.extensionOptionsError) throw new Error(providerOptionsChanged(provider));
  const owner = editingDraft.extensionOwner;
  if (!sameProviderOptionsOwner(owner, provider.extensionOptions)) throw new Error(providerOptionsChanged(provider));
  const values = editingDraft.extensionOptions || {};
  if (!Object.keys(values).length && editingDraft.extensionOptionsSaved) return;
  const submitted = Object.fromEntries(owner.options.map(option => [
    option.key, Object.hasOwn(values, option.key) ? values[option.key] : !!option.on,
  ]));
  let receipt;
  try {
    receipt = await api("provider/extension-options", {
      id: provider.id, module: owner.module, provider: owner.provider,
      packageRevision: owner.packageRevision, expected: owner.configVersion,
      values: submitted, confirmed: [...(editingDraft.extensionConfirms || [])],
    });
  } catch (error) {
    editingDraft.extensionOptionsUnknown = true;
    provider.extensionOptionsUnknown = true;
    throw new Error(`${error?.message || error}. ${providerOptionsChanged(provider)}`);
  }
  if (typeof receipt?.version !== "string" || !/^(0|[1-9]\d*)$/.test(receipt.version)) {
    editingDraft.extensionOptionsUnknown = true;
    provider.extensionOptionsUnknown = true;
    throw new Error(providerOptionsChanged(provider));
  }
  const currentOwner = sameProviderOptionsOwner(owner, provider.extensionOptions);
  for (const option of owner.options) {
    if (!Object.hasOwn(submitted, option.key)) continue;
    const wanted = Object.hasOwn(values, option.key) ? values[option.key] : !!option.on;
    option.on = submitted[option.key];
    if (wanted === option.on) delete values[option.key];
    else values[option.key] = wanted;
    if (!wanted || wanted === option.on) editingDraft.extensionConfirms?.delete(option.key);
  }
  owner.configVersion = receipt.version;
  editingDraft.extensionOptionsSaved = true;
  if (currentOwner) provider.extensionOptions = structuredClone(owner);
  if (Object.keys(values).length) throw new Error(t("Provider options saved; save again to include your newer edits."));
};

window.captureProviderOptionsSave = function(okMessage) {
  if (!draft?.extensionOwner) return null;
  const editingDraft = draft, saved = providerDraftValue();
  return async () => {
    if (draft === editingDraft && providerDraftValue() === saved) return false;
    renderProviders();
    state = await api("state");
    renderAgents();
    saidMoved(draft ? t("Provider saved; your newer edits are still open.") : okMessage);
    for (const button of document.querySelectorAll(".editor .busy")) doneSaving(button);
    return true;
  };
};
