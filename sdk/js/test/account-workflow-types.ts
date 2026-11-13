import { createAccountWorkflow, type AccountWorkflow, type AccountSelection, type AccountSummary, type WindowAuthFlowView } from "../ui.js";
import { type AccountSelection as GoSelection, type AccountSummary as GoSummary, type WindowAuthFlowView as GoFlow } from "../index.js";

const workflow: AccountWorkflow = createAccountWorkflow("inspect", "signin");
const account: AccountSelection = { id: "chosen", expectedGeneration: "18446744073709551615" };
const summary: AccountSummary = { id: account.id, generation: account.expectedGeneration };
const flow: WindowAuthFlowView = { flow: "host-issued", state: "complete", account: summary };
const goSelection: GoSelection = account;
const goSummary: GoSummary = summary;
const goFlow: GoFlow = flow;
void [goSelection, goSummary, goFlow];
const refreshed: Promise<readonly Readonly<AccountSummary>[]> = workflow.refresh();
const result: Promise<{ generation: string }> = workflow.call({ prompt: "one" });
void [refreshed, result];
workflow.select("chosen");
workflow.start({ method: "device", input: { project: "one" } }, true);
workflow.finish();
workflow.cancel();
// @ts-expect-error account scope comes from selection state, never from form options
workflow.start({ method: "device", account });
// @ts-expect-error selecting an object cannot supply a generation or authority
workflow.select(account);
// @ts-expect-error snapshot selection is immutable
workflow.snapshot.selection = account;
