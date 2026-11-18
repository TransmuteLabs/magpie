import { accountKeepAliveLead, type AccountKeepAliveDescriptor, type Manifest } from "../index.js";

const policy: AccountKeepAliveDescriptor = { leadMs: 60000 };
const lead: number = accountKeepAliveLead(policy);
const manifest: Manifest = {
  schema: 1, id: "fixture", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth"] },
  providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"], keepAlive: policy }],
};
const disabled: AccountKeepAliveDescriptor | null | undefined = manifest.providers?.[0].keepAlive;
accountKeepAliveLead(disabled);
void lead;
