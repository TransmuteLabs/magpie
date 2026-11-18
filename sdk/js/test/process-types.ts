import {
  type AgentProcessRecipe, type ProcessHostMethod, type ProcessReadResult,
  type Stream, processHostLane,
} from "../index.js";

const recipe: AgentProcessRecipe = { id: "run", agent: "worker-agent", argv: ["app-server", "--stdio"] };
const method: ProcessHostMethod = "process.read";
const lane: 0 | 1 | 2 | 3 = processHostLane(method, { handle: "opaque", pipe: "stdout", seq: "1", maxBytes: 64 });
async function read(stream: Stream): Promise<ProcessReadResult> {
  return stream.hostCall<ProcessReadResult>(method, { handle: "opaque", pipe: "stdout", seq: "1", maxBytes: 64 });
}
void recipe;
void lane;
void read;
