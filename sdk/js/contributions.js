import { validateRouting } from "./routing.js";
import { validateSearchSources } from "./search-sources.js";

const descriptorKeys = ["routing", "search", "libraryRule"];
const hasContent = value => typeof value === "string" && /\P{White_Space}/u.test(value);

export function validateContributions(manifest, reject, validID) {
  validateRouting(manifest, reject);
  validateSearchSources(manifest, reject);
  const contributions = manifest.contributions;
  if (contributions == null || Array.isArray(contributions) && contributions.length === 0) return;
  if (!Array.isArray(contributions) || contributions.length > 144 || manifest.component != null ||
    manifest.contract.version.minor < 16 || !manifest.contract.capabilities.includes("module-rpc")) {
    reject("Contributions require root SDK API 1.16 module-rpc within descriptor limits");
  }
  const routingStrategies = [...(manifest.routingStrategies ?? [])];
  const searchSources = [...(manifest.searchSources ?? [])];
  const rules = new Set();
  for (const declaration of contributions) {
    const field = declaration?.point === "library-rule" ? "libraryRule" : declaration?.point;
    if (declaration == null || typeof declaration !== "object" || Array.isArray(declaration) ||
      declaration.version !== 1 || !["search", "routing", "library-rule"].includes(declaration.point) ||
      declaration[field] == null ||
      descriptorKeys.some(key => key !== field && declaration[key] != null) ||
      Object.keys(declaration).some(key => key !== "point" && key !== "version" && !descriptorKeys.includes(key))) {
      reject("Invalid versioned owner contribution");
    }
    if (field === "libraryRule") {
      const rule = declaration.libraryRule;
      if (manifest.contract.version.minor < 17 || typeof rule !== "object" || Array.isArray(rule) ||
        Object.keys(rule).some(key => !["id", "body", "paths"].includes(key)) ||
        !validID(rule.id) || rules.has(rule.id) || rules.size >= 16 ||
        !hasContent(rule.body) || Buffer.byteLength(rule.body) > 100000 ||
        rule.body.includes("\0") || (rule.paths != null && (!Array.isArray(rule.paths) || rule.paths.length > 50 ||
          rule.paths.some(glob => !hasContent(glob) || Buffer.byteLength(glob) > 500 || /[\0\r\n]/.test(glob))))) {
        reject("Invalid Library rule contribution");
      }
      rules.add(rule.id);
    } else {
      (field === "search" ? searchSources : routingStrategies).push(declaration[field]);
    }
  }
  const projected = { ...manifest, routingStrategies, searchSources };
  validateRouting(projected, reject);
  validateSearchSources(projected, reject);
}
