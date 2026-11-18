import type { UsageWindow } from "../index.js";

const window: UsageWindow = {
  name: "Vendor units", used: 0, amount: 0, balance: null,
  limit: 10.25, display: "0 / 10.25", unit: "ACU", span: 18000,
};

const amount: number | null | undefined = window.amount;
const balance: number | null | undefined = window.balance;
const span: number | undefined = window.span;
void [amount, balance, span];
