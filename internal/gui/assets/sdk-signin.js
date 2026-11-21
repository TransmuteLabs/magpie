export async function start(sub, form, options) {
  if (sub.page) {
    const available = await window.loadPluginPages?.(sub.page);
    if (signing !== form) return;
    signing = null;
    if (!available || !await show(sub.page)) {
      renderProviders();
      statusErr(new Error(t("Plugin page is unavailable")));
    }
    return;
  }
  providerForm(sub, form, options);
}

export async function signOut(a, l, p) {
  const shown = { agent: p?.name || a.agentName, user: accountShownOf(p, l.user) };
  if (!await confirmAction(t("Sign {agent} out of {user}?", shown),
    t("magpie forgets this account's sign-in; the account itself is untouched"), t("Sign out"))) return;
  await accountAction("login/forget", { agent: a.agent, account: { id: l.user, expectedGeneration: l.extensionGeneration } },
    t("{agent} signed out of {user}", shown), true);
}

function choiceButton(label, choices, change) {
  const button = el("button", "proto");
  button.type = "button";
  button.setAttribute("aria-label", label);
  menuOf(button);
  const options = choices.map((choice) => ({ v: choice.value, name: choice.label, literalName: true }));
  const pick = (choice) => {
    button.value = choice.v;
    button.replaceChildren(el("span", "", choice.name), svg(CHEV, 11, 1.6));
  };
  pick(options[0]);
  button.onclick = (event) => {
    event.stopPropagation();
    if (button.classList.contains("open")) return closeProtoMenu();
    openProtoMenu(button, options, button.value, (value) => {
      pick(options.find((choice) => choice.v === value));
      button.focus(NO_SCROLL);
      change?.();
    }, label);
  };
  return button;
}

function providerForm(sub, form, options) {
  const parent = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, "0")).join("");
  const isCurrent = () => !closed && signing === form;
  const send = (operation, body) => api("provider/sdk-signin", {
    id: sub.id, revision: sub.revision, parent, operation, ...body,
  });
  const discard = (flow) => { if (flow) send("cancel", { flow }).catch(() => {}); };
  const account = options.user ? { id: options.user, expectedGeneration: options.generation } : null;
  let timer, handle, result, fault, openError, openedURL, pending = false, closed = false;
  form.cancel = () => {
    closed = true;
    clearTimeout(timer);
    discard(handle);
    handle = "";
  };

  const summary = el("span", "tt");
  const questions = el("form", "tt");
  const title = account ? t("Sign in again") + " · " + account.id : t("Sign in to {name}", { name: sub.name });
  const ask = el("span", "n", title);
  const fields = el("div");
  let controls = [];
  const submit = Object.assign(el("button", "text primary", t("Sign in")), { type: "submit" });
  const choose = () => {
    fields.replaceChildren();
    const method = sub.methods.find((method) => method.id === methods.value);
    controls = (method.fields || []).map((field) => {
      const label = el("label", "callback-form");
      let control;
      if (field.type === "select") {
        const choices = field.optional ? [{ value: "", label: t("None") }, ...field.choices] : field.choices;
        control = choiceButton(field.label, choices);
      } else {
        control = input("", "", field.type === "secret" ? "password" : "text");
        control.maxLength = 16 * 1024;
        control.required = !field.optional;
      }
      label.append(el("span", "n", field.label), control);
      fields.append(label);
      return { field, control };
    });
  };
  const methods = choiceButton(t("Sign-in method"), sub.methods.map((method) => ({ value: method.id, label: method.label })), choose);
  questions.append(ask);
  if (sub.methods.length > 1) questions.append(methods);
  questions.append(fields, el("span", "acts"));
  questions.lastChild.append(submit);
  choose();

  const open = async (url) => {
    if (!isCurrent()) return;
    try {
      await openSignInPage(url);
      openError = "";
    } catch (error) {
      openError = error.message === openBlocked
        ? t("Your browser blocked the sign-in page from opening. Open it with Open again below, or copy the link into a browser.")
        : error.message;
    }
    if (isCurrent()) renderProviders();
  };

  const fail = (message) => {
    clearTimeout(timer);
    discard(handle);
    handle = "";
    fault = message;
    renderProviders();
  };

  const advance = async (operation, body) => {
    if (!isCurrent()) return;
    try {
      const next = await send(operation, body);
      if (!isCurrent()) {
        if (operation === "start") discard(next?.flow);
        return;
      }
      result = next;
      handle = result.flow;
      if (result.state === "complete") {
        handle = "";
        closed = true;
        await signedIn({ agent: sub.agent, user: result.account.id, again: !!account });
        return;
      }
      if (result.state !== "waiting" && result.state !== "pending") {
        handle = "";
        fail(t("Sign-in didn't finish"));
        return;
      }
      if (result.url && !result.linkOnly && !options.linkOnly && result.url !== openedURL) {
        openedURL = result.url;
        open(result.url);
      }
      renderProviders();
      timer = setTimeout(() => advance("finish", { flow: handle }), Math.max(1000, result.retryAfterMs || 0));
    } catch (error) {
      if (isCurrent()) fail(error.message);
      else if (closed && result?.state === "complete") statusErr(error);
    }
  };
  questions.onsubmit = (event) => {
    event.preventDefault();
    if (!isCurrent() || pending) return;
    const inputs = Object.fromEntries(controls.flatMap(({ field, control }) => {
      const value = control.value;
      if (field.type !== "select") control.value = "";
      return value === "" && field.optional ? [] : [[field.key, value]];
    }));
    const grant = { method: methods.value, input: inputs };
    if (account) grant.account = account;
    pending = true;
    renderProviders();
    advance("start", { request: "start", options: grant });
  };

  form.draw = () => {
    const box = el("div", "signing" + (fault ? " failed" : ""));
    const cancel = el("button", "text", t("Cancel"));
    cancel.onclick = cancelSignIn;
    if (!pending && !fault) {
      box.append(questions, cancel);
      return box;
    }
    summary.replaceChildren(el("span", "n", fault ? t("Sign-in didn't finish") : title));
    if (fault) {
      summary.append(el("span", "s", fault));
      if (!account || sub.reauthorize && options.generation) {
        const again = el("button", "link", t("Try again"));
        again.onclick = async () => {
          try {
            providers = await api("providers");
            if (!isCurrent()) return;
            const fresh = subOf(sub.agent);
            const login = providers.providers.find((row) => row.account?.agent === sub.agent)
              ?.account.logins?.find((row) => row.user === options.user);
            if (!fresh?.revision || account && (!fresh.reauthorize || !login?.extensionGeneration)) {
              cancelSignIn();
              return statusErr(new Error(t(account ? "Sign-in could not use the selected account" : "Sign-in didn't finish")));
            }
            startSignIn(sub.agent, true, undefined, { ...options, generation: login?.extensionGeneration });
          } catch (error) { if (isCurrent()) statusErr(error); }
        };
        const actions = el("span", "acts");
        actions.append(again);
        summary.append(actions);
      }
    } else if (!result) {
      summary.append(el("span", "s", t("Starting the sign-in…")));
    } else {
      if (openError) summary.append(el("span", "s", openError));
      if (result.userCode) {
        const code = el("span", "devcode");
        code.append(el("code", "", result.userCode), copyBtn(result.userCode, t("Code")));
        summary.append(code);
      }
      if (result.url) {
        const link = el("span", "signlink");
        const openButton = el("button", "link", t(openError ? "Open again" : "Open in browser"));
        openButton.onclick = () => open(result.url);
        link.append(el("code", "", result.url), copyBtn(result.url, t("Sign-in link")), openButton);
        summary.append(link);
      }
    }
    box.append(summary, cancel);
    return box;
  };

  if (account && (!sub.reauthorize || !options.generation)) {
    fail(t("Sign-in could not use the selected account"));
  } else {
    renderProviders();
  }
}
