import { validateRouting } from "./routing.js";
import { validateSearchSources } from "./search-sources.js";

export function validateContributions(manifest, reject) {
  validateRouting(manifest, reject);
  validateSearchSources(manifest, reject);
  const contributions = manifest.contributions;
  if (contributions == null || Array.isArray(contributions) && contributions.length === 0) return;
  if (!Array.isArray(contributions) || contributions.length > 128 || manifest.component != null ||
    manifest.contract.version.minor < 16 || !manifest.contract.capabilities.includes("module-rpc")) {
    reject("Contributions require root SDK API 1.16 module-rpc within descriptor limits");
  }
  const routingStrategies = [...(manifest.routingStrategies ?? [])];
  const searchSources = [...(manifest.searchSources ?? [])];
  for (const declaration of contributions) {
    if (declaration == null || typeof declaration !== "object" || Array.isArray(declaration) ||
      declaration.version !== 1 || !["search", "routing"].includes(declaration.point) ||
      declaration[declaration.point] == null ||
      declaration[declaration.point === "search" ? "routing" : "search"] != null ||
      Object.keys(declaration).some(key => !["point", "version", "search", "routing"].includes(key))) {
      reject("Invalid versioned owner contribution");
    }
    (declaration.point === "search" ? searchSources : routingStrategies).push(declaration[declaration.point]);
  }
  const projected = { ...manifest, routingStrategies, searchSources };
  validateRouting(projected, reject);
  validateSearchSources(projected, reject);
}
