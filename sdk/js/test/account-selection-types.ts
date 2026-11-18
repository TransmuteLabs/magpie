import { type AccountSelection, type AccountSummary, type WindowCallOptions, validateAccountSelection } from "../index.js";
import { ui, type AccountSelection as BrowserAccountSelection } from "../ui.js";

const selection: AccountSelection = validateAccountSelection({ id: "account-a", expectedGeneration: "9007199254740993" });
const browser: BrowserAccountSelection = selection;
const options: WindowCallOptions = { account: browser };
const summaries: Promise<AccountSummary[]> = ui.backend.accounts("page.reset");
const answer: Promise<{ done: boolean }> = ui.backend.call("page.reset", { window: "week" }, options);
void summaries;
void answer;

// @ts-expect-error generation must not be a JavaScript number
const invalid: AccountSelection = { id: "account-a", expectedGeneration: 9007199254740993 };
void invalid;
