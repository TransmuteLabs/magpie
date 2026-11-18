interface SelectedAccount { id: string; generation: string }
interface AccountBridge {
  ready: Promise<unknown>;
  backend: {
    accounts(binding: string): Promise<SelectedAccount[]>;
    call(binding: string, input: unknown, options: { account: { id: string; expectedGeneration: string } }): Promise<unknown>;
  };
}

declare const magpie: AccountBridge;

const picker = document.querySelector<HTMLSelectElement>("#account")!;
const output = document.querySelector("pre")!;
let accounts: SelectedAccount[] = [];

async function refresh(): Promise<void> {
  picker.replaceChildren(new Option("Choose an account", ""));
  try {
    await magpie.ready;
    accounts = await magpie.backend.accounts("page.pid");
    for (const account of accounts) picker.add(new Option(account.id, account.id));
    output.textContent = "Select the account for this action.";
  } catch (error) {
    accounts = [];
    output.textContent = error instanceof Error ? error.message : String(error);
  }
}

async function invoke(binding: string): Promise<void> {
  try {
    const account = accounts.find(value => value.id === picker.value);
    if (!account) throw new Error("Choose an account first.");
    const input = { text: document.querySelector<HTMLInputElement>("#text")!.value };
    const options = { account: { id: account.id, expectedGeneration: account.generation } };
    output.textContent = JSON.stringify(await magpie.backend.call(binding, input, options), null, 2);
  } catch (error) {
    output.textContent = error instanceof Error ? error.message : String(error);
  }
}

document.querySelector("#refresh")!.addEventListener("click", () => void refresh());
document.querySelector("#primary")!.addEventListener("click", () => void invoke("page.pid"));
document.querySelector("#backend")!.addEventListener("click", () => void invoke("page.backend"));
document.querySelector("#forward")!.addEventListener("click", () => void invoke("page.forward"));
void refresh();
