import {
  type ContributionDescriptor,
  type Manifest,
  type RoutingFlags,
  type RoutingRequest,
  type RoutingResult,
  type RoutingSeat,
  type RoutingStrategyDescriptor,
  validateRoutingRequest,
  validateRoutingResult,
} from "../index.js";

const flags: RoutingFlags = { exact: true, byUse: true, byFree: true, byPlan: true, bySpeed: true, byPrice: true };
const descriptor: RoutingStrategyDescriptor = { id: "reverse", name: "Reverse", action: "rank", flags, timeoutMs: 500 };
const seat: RoutingSeat = { key: "opaque", weight: 1, soon: [1234], unreached: 1, cost: 0 };
const request: RoutingRequest = { strategy: descriptor.id, pool: "provider", seats: [seat] };
const result: RoutingResult = validateRoutingResult({ order: [0] }, validateRoutingRequest(request).seats.length);
const projection: Pick<Manifest, "routingStrategies"> = { routingStrategies: [descriptor] };
const command: ContributionDescriptor = {
  point: "library-command",
  version: 1,
  libraryCommand: { id: "review", description: "Review changes", prompt: "Review $1" },
};
void result;
void projection;
void command;

// @ts-expect-error an order contains finite indexes, not keys
const invalid: RoutingResult = { order: ["opaque"] };
void invalid;
