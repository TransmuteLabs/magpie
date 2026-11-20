import { validateRouting } from "./routing.js";
import { validateSearchSources } from "./search-sources.js";

const descriptorFields = new Map([
  ["routing", "routing"],
  ["search", "search"],
  ["library-rule", "libraryRule"],
  ["library-command", "libraryCommand"],
]);
const descriptorKeys = [...descriptorFields.values()];
const hasContent = value => typeof value === "string" && /\P{White_Space}/u.test(value);

export function validateContributions(manifest, reject, validID) {
  validateRouting(manifest, reject);
  validateSearchSources(manifest, reject);
  const contributions = manifest.contributions;
  if (contributions == null || Array.isArray(contributions) && contributions.length === 0) return;
  if (!Array.isArray(contributions) || contributions.length > 160 || manifest.component != null ||
    manifest.contract.version.minor < 16 || !manifest.contract.capabilities.includes("module-rpc")) {
    reject("Contributions require root SDK API 1.16 module-rpc within descriptor limits");
  }
  const routingStrategies = [...(manifest.routingStrategies ?? [])];
  const searchSources = [...(manifest.searchSources ?? [])];
  const rules = new Set();
  const commands = new Set();
  for (const declaration of contributions) {
    const field = descriptorFields.get(declaration?.point);
    if (declaration == null || typeof declaration !== "object" || Array.isArray(declaration) ||
      declaration.version !== 1 || !field ||
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
    } else if (field === "libraryCommand") {
      const command = declaration.libraryCommand;
      if (manifest.contract.version.minor < 18 || typeof command !== "object" || Array.isArray(command) ||
        Object.keys(command).some(key => !["id", "description", "prompt"].includes(key)) ||
        !validID(command.id) || commands.has(command.id) || commands.size >= 16 ||
        (command.description != null && (typeof command.description !== "string" ||
          [...command.description].length > 1000 || command.description.includes("\0"))) ||
        !hasContent(command.prompt) || Buffer.byteLength(command.prompt) > 100000 || command.prompt.includes("\0")) {
        reject("Invalid Library command contribution");
      }
      commands.add(command.id);
    } else {
      (field === "search" ? searchSources : routingStrategies).push(declaration[field]);
    }
  }
  const projected = { ...manifest, routingStrategies, searchSources };
  validateRouting(projected, reject);
  validateSearchSources(projected, reject);
}
