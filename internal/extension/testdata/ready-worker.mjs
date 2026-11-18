import { serve } from "../../../sdk/js/index.js";
await serve({ manifest: JSON.parse(process.argv[2]), start() { return { endpoints: { ui: process.argv[3] } }; } });
