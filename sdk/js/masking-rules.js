import { SDKError } from "./bytes.js";

const kind = /^SDK_[A-Z](?:[A-Z0-9_]{0,18}[A-Z0-9])?$/;
const prefix = /^[A-Za-z][A-Za-z0-9_:/.-]{2,63}$/;
const regex = /^[A-Za-z][A-Za-z0-9_:/-]{2,63}\[[A-Za-z0-9_-]{1,64}\]\{([1-9][0-9]{0,2})(?:,([1-9][0-9]{0,2}))?\}$/;
const object = (value, fields) => value !== null && typeof value === "object" && !Array.isArray(value)
  && Object.keys(value).every(field => fields.includes(field));
const invalid = () => { throw new SDKError("invalid_params", "Invalid declarative masking rule set"); };

export function validateMaskingRuleSets(manifest, validID) {
  const declared = manifest.contract.capabilities.includes("masking-rules");
  const sets = manifest.maskingRuleSets ?? [];
  if (!declared && Array.isArray(sets) && !sets.length) return;
  if (!declared || manifest.component != null || manifest.contract.version.minor < 15
    || !Array.isArray(sets) || !sets.length || sets.length > 8) invalid();
  const ids = new Set();
  for (const set of sets) {
    if (!object(set, ["id", "rules"]) || !validID(set.id) || ids.has(set.id)
      || !Array.isArray(set.rules) || !set.rules.length || set.rules.length > 8) invalid();
    ids.add(set.id);
    const kinds = new Set();
    for (const rule of set.rules) {
      if (!object(rule, ["kind", "prefix", "regex"]) || typeof rule.kind !== "string" || !kind.test(rule.kind)
        || kinds.has(rule.kind)) invalid();
      kinds.add(rule.kind);
      const literal = rule.prefix ?? "", pattern = rule.regex ?? "";
      if (typeof literal !== "string" || typeof pattern !== "string" || !literal === !pattern) invalid();
      if (literal) {
        if (!prefix.test(literal)) invalid();
        continue;
      }
      const match = regex.exec(pattern);
      if (!match || +match[1] < 4 || +(match[2] ?? match[1]) < +match[1] || +(match[2] ?? match[1]) > 128) invalid();
      try { new RegExp(pattern); } catch { invalid(); }
    }
  }
}
