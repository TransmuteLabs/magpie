export async function uninstallMagpie(button = $("#uninstallRow button")) {
  if (!button) return;
  if (!(await confirmAction(t("Uninstall magpie?"), t("Every agent is disconnected and gets back what it had before magpie; what you changed by hand since stays. magpie's background service, opening at login and shortcuts are removed, and once it quits, its providers, keys, settings and usage are deleted. The app itself stays where it is: delete it to finish."), t("Uninstall")))) return;
  button.disabled = true;
  try {
    const result = await api("uninstall", {});
    if (result.failed) {
      const left = result.says.filter((line) => line.startsWith("!")).map((line) => line.slice(2)).join("; ");
      if (await confirmAction(t("Some of magpie couldn't be taken off"), t("Nothing else was removed, and magpie keeps running. What is left: {left}", { left }), t("Try again"))) return uninstallMagpie(button);
      return;
    }
    if (result.codexApp && !(await confirmAction(t("Open the Codex app again"), t("The Codex app is still on the settings it read before: quit it and open it again. magpie quits now, and its files are deleted once it has."), t("Quit magpie")))) return;
    await api("uninstall/finish", {});
  } catch (error) {
    statusTErr(error);
  } finally {
    if (button.isConnected) button.disabled = false;
  }
}
