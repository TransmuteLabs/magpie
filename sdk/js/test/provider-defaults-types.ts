import type { Manifest } from "../index.js";

const provider: NonNullable<Manifest["providers"]>[number] = {
  id: "vendor", name: "Vendor", protocols: ["chat"],
  maxConcurrency: 3, maxAccounts: 0,
};

const perAccount: number | undefined = provider.maxConcurrency;
const activeAccounts: number | undefined = provider.maxAccounts;
void [perAccount, activeAccounts];
